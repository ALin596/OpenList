package xiaomi_cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/go-resty/resty/v2"
	log "github.com/sirupsen/logrus"
)

type XiaomiCloud struct {
	model.Storage
	Addition

	sessionMu    sync.RWMutex
	refreshMu    sync.Mutex
	recentFileMu sync.Mutex
	session      sessionCookies
	recentFiles  map[string]map[string]recentFile
	// 由 refreshMu 保护，保存服务端轮换的凭据，不参与存储配置序列化。
	runtimeAccountCookie string
}

func (d *XiaomiCloud) Config() driver.Config {
	return config
}

func (d *XiaomiCloud) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *XiaomiCloud) Init(ctx context.Context) error {
	d.UploadThread = normalizeUploadThreads(d.UploadThread)
	if strings.TrimSpace(d.AccountCookie) == "" {
		return fmt.Errorf("account_cookie 不能为空")
	}
	if !hasCookie(d.AccountCookie, "passToken") {
		return fmt.Errorf("account_cookie 中缺少 passToken")
	}
	if err := d.ensureServiceToken(ctx, ""); err != nil {
		return normalizeError(err)
	}

	// 验证Cookie有效性
	apiURL := fmt.Sprintf("%s/user/status", baseURL)
	var statusResp struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}

	err := d.request(ctx, apiURL, http.MethodGet, nil, &statusResp)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			return errs.NewErr(errs.EmptyToken, "serviceToken 验证失败，请检查 account_cookie: %v", err)
		}
		return normalizeError(err)
	}

	if statusResp.Data.Status != "NORMAL" {
		return fmt.Errorf("账户状态异常: %s", statusResp.Data.Status)
	}

	log.Infof("[xiaomi_cloud] 驱动初始化成功,账户状态正常")
	return nil
}

func (d *XiaomiCloud) Drop(_ context.Context) error {
	d.refreshMu.Lock()
	defer d.refreshMu.Unlock()

	// 重新配置或停用后丢弃旧账号凭据，下次初始化使用用户当前保存的配置。
	d.runtimeAccountCookie = ""
	d.sessionMu.Lock()
	d.session = sessionCookies{}
	d.sessionMu.Unlock()

	d.recentFileMu.Lock()
	d.recentFiles = nil
	d.recentFileMu.Unlock()
	return nil
}

func (d *XiaomiCloud) List(ctx context.Context, dir model.Obj, _ model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(ctx, dir.GetID())
	if err != nil {
		return nil, normalizeError(err)
	}
	files = d.mergeRecentFiles(dir.GetID(), files)

	return utils.SliceConvert(files, func(src file) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *XiaomiCloud) Link(ctx context.Context, file model.Obj, _ model.LinkArgs) (*model.Link, error) {
	fileInfo, err := d.getFile(ctx, file.GetID())
	if err != nil {
		return nil, normalizeError(err)
	}

	// 检查URL是否过期
	if fileInfo.Storage.ExpireAt < time.Now().UnixMilli() {
		return nil, fmt.Errorf("下载链接已过期,请重试")
	}

	return &model.Link{
		URL: fileInfo.Storage.DownloadURL,
		Header: http.Header{
			"Referer":    []string{referer},
			"User-Agent": []string{userAgent},
		},
	}, nil
}

func (d *XiaomiCloud) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) (model.Obj, error) {
	apiURL := fmt.Sprintf("%s%s/folders/create", baseURL, apiPrefix)

	var resp createFolderResponse
	err := d.request(ctx, apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetFormData(map[string]string{
			"name":         dirName,
			"parentId":     parentDir.GetID(),
			"serviceToken": serviceToken,
		})
	}, &resp)

	if err != nil {
		return nil, normalizeError(err)
	}
	if resp.Data.ID == "" {
		return nil, fmt.Errorf("创建文件夹响应缺少目录ID")
	}
	if resp.Data.Name == "" {
		resp.Data.Name = dirName
	}
	if resp.Data.ParentID == "" {
		resp.Data.ParentID = flexibleID(parentDir.GetID())
	}
	if resp.Data.Type == "" {
		resp.Data.Type = "folder"
	}

	d.rememberRecentFile(parentDir.GetID(), resp.Data)
	log.Infof("[xiaomi_cloud] 创建文件夹成功: %s (ID: %s)", dirName, resp.Data.ID)
	return fileToObj(resp.Data), nil
}

func (d *XiaomiCloud) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	apiURL := fmt.Sprintf("%s%s/records/filemanager", baseURL, apiPrefix)

	operateRecords := []fileManagerRecord{
		{
			ID:       srcObj.GetID(),
			ParentID: dstDir.GetID(),
			Type:     getFileManagerRecordType(srcObj),
		},
	}
	operateRecordsJSON, err := json.Marshal(operateRecords)
	if err != nil {
		return fmt.Errorf("构建移动请求失败: %w", err)
	}

	var resp fileManagerResponse
	err = d.request(ctx, apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetFormData(map[string]string{
			"operateType":    "MOVE",
			"operateRecords": string(operateRecordsJSON),
			"serviceToken":   serviceToken,
		})
	}, &resp)

	if err != nil {
		return normalizeError(err)
	}

	if resp.Data.TaskID != "" {
		if err = d.waitForTask(ctx, resp.Data.TaskID, 30*time.Second); err != nil {
			return fmt.Errorf("移动任务失败: %w", err)
		}
	}
	d.moveRecentFile(srcObj.GetID(), dstDir.GetID())

	return nil
}

func (d *XiaomiCloud) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	apiURL := fmt.Sprintf("%s%s/records/rename", baseURL, apiPrefix)

	err := d.request(ctx, apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetFormData(map[string]string{
			"id":           srcObj.GetID(),
			"newName":      newName,
			"channel":      "WEB",
			"serviceToken": serviceToken,
		})
	}, nil)

	if err != nil {
		return normalizeError(err)
	}
	d.renameRecentFile(srcObj.GetID(), newName)

	return nil
}

func (d *XiaomiCloud) Copy(_ context.Context, _, _ model.Obj) error {
	// 小米云盘没有直接的复制接口,需要通过下载再上传实现
	return errs.NewErr(errs.NotImplement, "小米云盘暂不支持复制操作")
}

func (d *XiaomiCloud) Remove(ctx context.Context, obj model.Obj) error {
	apiURL := fmt.Sprintf("%s%s/records/filemanager", baseURL, apiPrefix)

	// 构建操作记录
	operateRecords := []fileManagerRecord{
		{
			ID:   obj.GetID(),
			Type: getFileManagerRecordType(obj),
		},
	}

	operateRecordsJSON, err := json.Marshal(operateRecords)
	if err != nil {
		return fmt.Errorf("构建删除请求失败: %w", err)
	}

	var resp fileManagerResponse
	err = d.request(ctx, apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetFormData(map[string]string{
			"operateType":    "DELETE",
			"operateRecords": string(operateRecordsJSON),
			"serviceToken":   serviceToken,
		})
	}, &resp)

	if err != nil {
		return normalizeError(err)
	}

	// 等待异步任务完成
	if resp.Data.TaskID != "" {
		log.Infof("[xiaomi_cloud] 删除任务已创建: %s,等待完成...", resp.Data.TaskID)
		err = d.waitForTask(ctx, resp.Data.TaskID, 30*time.Second)
		if err != nil {
			return fmt.Errorf("删除任务失败: %w", err)
		}
	}
	d.forgetRecentFile(obj.GetID())

	return nil
}

func (d *XiaomiCloud) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	apiURL := fmt.Sprintf("%s/status/alldetail", baseURL)

	var resp storageInfoResponse
	err := d.request(ctx, apiURL, http.MethodGet, nil, &resp)
	if err != nil {
		return nil, normalizeError(err)
	}

	return &model.StorageDetails{
		DiskUsage: model.DiskUsage{
			TotalSpace: resp.Data.TotalQuota,
			UsedSpace:  resp.Data.Used,
		},
	}, nil
}

var _ driver.Driver = (*XiaomiCloud)(nil)
var _ driver.MkdirResult = (*XiaomiCloud)(nil)
var _ driver.PutResult = (*XiaomiCloud)(nil)

func getFileManagerRecordType(obj model.Obj) string {
	if obj.IsDir() {
		return "folder"
	}
	if strings.HasPrefix(utils.GetMimeType(obj.GetName()), "image/") {
		return "pic"
	}
	return "other"
}
