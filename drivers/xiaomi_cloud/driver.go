package xiaomi_cloud

import (
	"context"
	"encoding/json"
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
	serviceToken string
	userID       string
	sessionHash  string
	userHash     string
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
	apiURL := fmt.Sprintf("%s/user/status", BaseURL)
	var statusResp struct {
		Result string `json:"result"`
		Code   int    `json:"code"`
		Data   struct {
			Status string `json:"status"`
		} `json:"data"`
	}

	_, err := d.request(ctx, apiURL, http.MethodGet, nil, &statusResp)
	if err != nil {
		return normalizeError(err)
	}

	if statusResp.Code != 0 {
		return errs.NewErr(errs.EmptyToken, "serviceToken 验证失败，请检查 account_cookie")
	}

	if statusResp.Data.Status != "NORMAL" {
		return fmt.Errorf("账户状态异常: %s", statusResp.Data.Status)
	}

	log.Infof("[xiaomi_cloud] 驱动初始化成功,账户状态正常")
	return nil
}

func (d *XiaomiCloud) Drop(ctx context.Context) error {
	// 清理资源(如果有需要)
	return nil
}

func (d *XiaomiCloud) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(ctx, dir.GetID())
	if err != nil {
		return nil, normalizeError(err)
	}

	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *XiaomiCloud) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	apiURL := fmt.Sprintf("%s%s/files/%s", BaseURL, APIPrefix, file.GetID())

	var resp FileInfoResp
	_, err := d.request(ctx, apiURL, http.MethodGet, nil, &resp)
	if err != nil {
		return nil, normalizeError(err)
	}

	if resp.Code != 0 {
		return nil, fmt.Errorf("获取下载链接失败: %s", resp.Description)
	}

	// 检查URL是否过期
	if resp.Data.Storage.ExpireAt < time.Now().UnixMilli() {
		return nil, fmt.Errorf("下载链接已过期,请重试")
	}

	return &model.Link{
		URL: resp.Data.Storage.DownloadURL,
		Header: http.Header{
			"Referer":    []string{Referer},
			"User-Agent": []string{UserAgent},
		},
	}, nil
}

func (d *XiaomiCloud) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	apiURL := fmt.Sprintf("%s%s/folders/create", BaseURL, APIPrefix)

	var resp CreateFolderResp
	_, err := d.request(ctx, apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
		req.SetBody(buildFormData(map[string]string{
			"name":         dirName,
			"parentId":     parentDir.GetID(),
			"serviceToken": serviceToken,
		}))
	}, &resp)

	if err != nil {
		return normalizeError(err)
	}

	if resp.Code != 0 {
		return fmt.Errorf("创建文件夹失败: %s", resp.Description)
	}

	log.Infof("[xiaomi_cloud] 创建文件夹成功: %s (ID: %s)", dirName, resp.Data.ID)
	return nil
}

func (d *XiaomiCloud) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	apiURL := fmt.Sprintf("%s%s/records/filemanager", BaseURL, APIPrefix)

	operateRecords := []FileManagerRecord{
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

	var resp DeleteResp
	_, err = d.request(ctx, apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
		req.SetBody(buildFormData(map[string]string{
			"operateType":    "MOVE",
			"operateRecords": string(operateRecordsJSON),
			"serviceToken":   serviceToken,
		}))
	}, &resp)

	if err != nil {
		return normalizeError(err)
	}

	if resp.Code != 0 {
		return fmt.Errorf("移动文件失败: %s", resp.Description)
	}

	if resp.Data.TaskID != "" {
		if err = d.waitForTask(ctx, resp.Data.TaskID, 30*time.Second); err != nil {
			return fmt.Errorf("移动任务失败: %w", err)
		}
	}

	return nil
}

func (d *XiaomiCloud) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	apiURL := fmt.Sprintf("%s%s/records/rename", BaseURL, APIPrefix)

	var resp RenameResp
	_, err := d.request(ctx, apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
		req.SetBody(buildFormData(map[string]string{
			"id":           srcObj.GetID(),
			"newName":      newName,
			"channel":      "WEB",
			"serviceToken": serviceToken,
		}))
	}, &resp)

	if err != nil {
		return normalizeError(err)
	}

	if resp.Code != 0 {
		return fmt.Errorf("重命名失败: %s", resp.Description)
	}

	return nil
}

func (d *XiaomiCloud) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	// 小米云盘没有直接的复制接口,需要通过下载再上传实现
	return errs.NewErr(errs.NotImplement, "小米云盘暂不支持复制操作")
}

func (d *XiaomiCloud) Remove(ctx context.Context, obj model.Obj) error {
	apiURL := fmt.Sprintf("%s%s/records/filemanager", BaseURL, APIPrefix)

	// 构建操作记录
	operateRecords := []FileManagerRecord{
		{
			ID:   obj.GetID(),
			Type: getFileManagerRecordType(obj),
		},
	}

	operateRecordsJSON, err := json.Marshal(operateRecords)
	if err != nil {
		return fmt.Errorf("构建删除请求失败: %w", err)
	}

	var resp DeleteResp
	_, err = d.request(ctx, apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
		req.SetBody(buildFormData(map[string]string{
			"operateType":    "DELETE",
			"operateRecords": string(operateRecordsJSON),
			"serviceToken":   serviceToken,
		}))
	}, &resp)

	if err != nil {
		return normalizeError(err)
	}

	if resp.Code != 0 {
		return fmt.Errorf("删除失败: %s", resp.Description)
	}

	// 等待异步任务完成
	if resp.Data.TaskID != "" {
		log.Infof("[xiaomi_cloud] 删除任务已创建: %s,等待完成...", resp.Data.TaskID)
		err = d.waitForTask(ctx, resp.Data.TaskID, 30*time.Second)
		if err != nil {
			return fmt.Errorf("删除任务失败: %w", err)
		}
	}

	return nil
}

func (d *XiaomiCloud) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	apiURL := fmt.Sprintf("%s/status/alldetail", BaseURL)

	var resp StorageInfoResp
	_, err := d.request(ctx, apiURL, http.MethodGet, nil, &resp)
	if err != nil {
		return nil, normalizeError(err)
	}

	if resp.Code != 0 {
		return nil, fmt.Errorf("获取存储空间失败: %s", resp.Description)
	}

	return &model.StorageDetails{
		DiskUsage: model.DiskUsage{
			TotalSpace: resp.Data.TotalQuota,
			UsedSpace:  resp.Data.Used,
		},
	}, nil
}

var _ driver.Driver = (*XiaomiCloud)(nil)

func getFileManagerRecordType(obj model.Obj) string {
	if obj.IsDir() {
		return "folder"
	}
	if strings.HasPrefix(utils.GetMimeType(obj.GetName()), "image/") {
		return "pic"
	}
	return "other"
}
