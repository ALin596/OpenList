package xiaomi_cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	xiaominet "github.com/OpenListTeam/OpenList/v4/internal/net"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
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
}

func (d *XiaomiCloud) Config() driver.Config {
	return config
}

func (d *XiaomiCloud) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *XiaomiCloud) Init(ctx context.Context) error {
	d.UploadThread = normalizeUploadThreads(d.UploadThread)
	if err := d.ensureServiceToken(ctx, ""); err != nil {
		return normalizeError(err)
	}

	// 仅当云盘会话有效且账号状态正常时，存储才进入可用状态。
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
			return errs.NewErr(errs.EmptyToken, "serviceToken 验证失败，请检查 passToken 和 userId: %v", err)
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
	download, err := d.getDownloadInfo(ctx, file.GetID())
	if err != nil {
		return nil, normalizeError(err)
	}

	// 下载地址和表单 meta 有效期相同，缓存不得超过小米返回的失效时间。
	expiration := time.Until(time.UnixMilli(download.ExpireAt))
	if expiration <= 0 {
		return nil, fmt.Errorf("下载链接已过期,请重试")
	}

	return &model.Link{
		RangeReader:   stream.RateLimitRangeReaderFunc(downloadRangeReader(file.GetID(), download, file.GetSize())),
		ContentLength: file.GetSize(),
		Expiration:    &expiration,
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

func downloadRangeReader(fileID string, download xiaomiDownload, size int64) stream.RangeReaderFunc {
	return func(ctx context.Context, requested http_range.Range) (io.ReadCloser, error) {
		if requested.Length < 0 || requested.Start+requested.Length > size {
			requested.Length = size - requested.Start
		}

		req := base.RestyClient.R().
			SetContext(ctx).
			SetHeaders(map[string]string{
				"Referer":    referer,
				"User-Agent": userAgent,
			}).
			SetFormData(map[string]string{"meta": download.Meta}).
			SetDoNotParseResponse(true)
		if requested.Length > 0 {
			headers := http_range.ApplyRangeToHttpHeader(requested, make(http.Header))
			req.SetHeader("Range", headers.Get("Range"))
		}

		// 小米网页把大块下载 meta 放在 POST 表单体中，避免它进入受长度限制的 URL。
		response, err := req.Post(download.URL)
		if err != nil {
			log.Errorf("[xiaomi_cloud] POST 下载失败: file_id=%s, range_start=%d, error=%s", fileID, requested.Start, safeErrorForLog(err))
			return nil, fmt.Errorf("提交小米下载表单失败: %w", err)
		}
		body := response.RawBody()
		if response.StatusCode() == http.StatusPartialContent {
			if got, want := response.RawResponse.Header.Get("Content-Range"), requested.ContentRange(size); got != want {
				body.Close()
				err := fmt.Errorf("小米云盘范围下载响应不匹配: got %q, want %q", got, want)
				log.Errorf("[xiaomi_cloud] POST 下载范围异常: file_id=%s, range_start=%d, error=%s", fileID, requested.Start, err)
				return nil, err
			}
			return body, nil
		}
		if response.StatusCode() != http.StatusOK {
			body.Close()
			err := &httpStatusError{code: response.StatusCode(), status: response.Status()}
			log.Errorf("[xiaomi_cloud] POST 下载被拒绝: file_id=%s, range_start=%d, error=%v", fileID, requested.Start, err)
			return nil, err
		}

		// 部分小米下载节点忽略 Range；保留同一份流式 POST 响应并从请求偏移读取。
		rangedBody, err := xiaominet.GetRangedHttpReader(body, requested.Start, requested.Length)
		if err != nil {
			body.Close()
			log.Errorf("[xiaomi_cloud] 读取 POST 下载范围失败: file_id=%s, range_start=%d, error=%s", fileID, requested.Start, safeErrorForLog(err))
			return nil, err
		}
		return rangedBody, nil
	}
}

func getFileManagerRecordType(obj model.Obj) string {
	if obj.IsDir() {
		return "folder"
	}
	if strings.HasPrefix(utils.GetMimeType(obj.GetName()), "image/") {
		return "pic"
	}
	return "other"
}
