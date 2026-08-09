package xiaomi_cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	if strings.TrimSpace(d.AccountCookie) == "" {
		return fmt.Errorf("account_cookie 不能为空")
	}
	if !hasCookie(d.AccountCookie, "passToken") {
		return fmt.Errorf("account_cookie 中缺少 passToken")
	}
	if err := d.ensureServiceToken(""); err != nil {
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

	_, err := d.request(apiURL, http.MethodGet, nil, &statusResp)
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
	files, err := d.getFiles(dir.GetID())
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
	_, err := d.request(apiURL, http.MethodGet, nil, &resp)
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
	_, err := d.request(apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
		req.SetBody(buildFormData(map[string]interface{}{
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
	_, err = d.request(apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
		req.SetBody(buildFormData(map[string]interface{}{
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
		if err = d.waitForTask(resp.Data.TaskID, 30*time.Second); err != nil {
			return fmt.Errorf("移动任务失败: %w", err)
		}
	}

	return nil
}

func (d *XiaomiCloud) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	apiURL := fmt.Sprintf("%s%s/records/rename", BaseURL, APIPrefix)

	var resp RenameResp
	_, err := d.request(apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
		req.SetBody(buildFormData(map[string]interface{}{
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
	_, err = d.request(apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
		req.SetBody(buildFormData(map[string]interface{}{
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
		err = d.waitForTask(resp.Data.TaskID, 30*time.Second)
		if err != nil {
			return fmt.Errorf("删除任务失败: %w", err)
		}
	}

	return nil
}

func (d *XiaomiCloud) Put(ctx context.Context, dstDir model.Obj, fileStreamer model.FileStreamer, up driver.UpdateProgress) error {
	// 计算文件总大小
	fileSize := fileStreamer.GetSize()
	fileName := fileStreamer.GetName()

	// 读取完整文件内容用于计算SHA1(小米云盘需要整个文件的SHA1)
	tempData, err := io.ReadAll(fileStreamer)
	if err != nil {
		return fmt.Errorf("读取文件失败: %w", err)
	}

	// 计算文件SHA1
	fileSHA1, err := calculateSHA1(bytes.NewReader(tempData))
	if err != nil {
		return fmt.Errorf("计算SHA1失败: %w", err)
	}

	// 计算分块数量
	blockCount := int((fileSize + ChunkSize - 1) / ChunkSize)
	blockInfos := make([]BlockInfo, 0, blockCount)

	// 计算每个分块的SHA1和MD5
	for i := 0; i < blockCount; i++ {
		start := int64(i) * ChunkSize
		end := start + ChunkSize
		if end > fileSize {
			end = fileSize
		}

		blockData := tempData[start:end]
		blockSHA1, blockMD5 := calculateBlockHash(blockData)

		blockInfos = append(blockInfos, BlockInfo{
			Sha1: blockSHA1,
			MD5:  blockMD5,
			Size: int64(len(blockData)),
		})
	}

	// 构建上传文件元数据
	uploadData := UploadFileData{
		FileName:        fileName,
		Size:            fileSize,
		Sha1:            fileSHA1,
		MimeType:        fileStreamer.GetMimetype(),
		Channel:         "WEB",
		ParentID:        dstDir.GetID(),
		LocalCreateTime: time.Now().UnixMilli(),
		LocalModifyTime: time.Now().UnixMilli(),
		ExifInfo:        make(map[string]interface{}),
	}

	kssInfo := KssInfo{
		BlockInfos: blockInfos,
		SplitSize:  ChunkSize,
	}

	uploadDataJSON, _ := json.Marshal(uploadData)
	kssInfoJSON, _ := json.Marshal(kssInfo)

	// 创建上传任务
	apiURL := fmt.Sprintf("%s%s/files/create", BaseURL, APIPrefix)
	var createResp CreateUploadResp
	err = d.retryRequest(3, func() error {
		createResp = CreateUploadResp{}
		_, requestErr := d.request(apiURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
			req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
			req.SetBody(buildFormData(map[string]interface{}{
				"data":                    string(uploadDataJSON),
				"kss":                     string(kssInfoJSON),
				"temp":                    false,
				"isClientUploadThumbnail": false,
				"serviceToken":            serviceToken,
			}))
		}, &createResp)
		return requestErr
	})

	if err != nil {
		return normalizeError(err)
	}

	if createResp.Code != 0 {
		return fmt.Errorf("创建上传任务失败: %s", createResp.Description)
	}

	// 检查是否秒传成功
	if createResp.Data.Exists {
		log.Infof("[xiaomi_cloud] 文件秒传成功: %s (ID: %s)", fileName, createResp.Data.ID)
		up(100)
		return nil
	}

	// 需要实际上传文件
	log.Infof("[xiaomi_cloud] 文件未秒传,开始分块上传: %s (%d 块)", fileName, blockCount)

	// 提取上传所需的参数
	uploadID := createResp.Data.UploadID
	fileMeta := createResp.Data.Kss.FileMeta
	if len(createResp.Data.Kss.NodeURLs) == 0 {
		return fmt.Errorf("创建上传任务未返回KSS节点地址")
	}
	nodeURL := createResp.Data.Kss.NodeURLs[0]
	blockMetas := createResp.Data.Kss.BlockMetas
	if uploadID == "" || fileMeta == "" {
		return fmt.Errorf("创建上传任务返回的KSS元数据不完整")
	}

	if len(blockMetas) != blockCount {
		return fmt.Errorf("分块数量不匹配: 预期 %d, 实际 %d", blockCount, len(blockMetas))
	}

	// 记录所有块的commit_meta
	commitMetas := make([]CommitMeta, 0, blockCount)

	// 上传每个分块
	for i := 0; i < blockCount; i++ {
		start := int64(i) * ChunkSize
		end := start + ChunkSize
		if end > fileSize {
			end = fileSize
		}

		blockData := tempData[start:end]
		blockMeta := blockMetas[i].BlockMeta
		if blockMeta == "" {
			return fmt.Errorf("分块 %d 未返回block_meta", i+1)
		}

		log.Infof("[xiaomi_cloud] 上传分块 %d/%d (大小: %d 字节)", i+1, blockCount, len(blockData))

		// 构建上传URL
		uploadURL := buildKSSUploadURL(nodeURL, fileMeta, blockMeta)

		// 上传分块
		var blockResp UploadBlockResp
		err = d.retryRequest(3, func() error {
			blockResp = UploadBlockResp{}
			_, requestErr := d.request(uploadURL, http.MethodPost, func(req *resty.Request, _ string) {
				req.SetHeader("Content-Type", "application/octet-stream")
				req.SetBody(blockData)
			}, &blockResp)
			return requestErr
		})

		if err != nil {
			return fmt.Errorf("上传分块 %d 失败: %w", i+1, err)
		}

		// 检查分块上传状态
		if blockResp.Stat != "BLOCK_COMPLETED" {
			return fmt.Errorf("分块 %d 上传状态异常: %s", i+1, blockResp.Stat)
		}

		// 记录commit_meta
		if blockResp.CommitMeta == "" {
			return fmt.Errorf("分块 %d 未返回commit_meta", i+1)
		}

		commitMetas = append(commitMetas, CommitMeta{
			CommitMeta: blockResp.CommitMeta,
		})

		// 更新进度
		progress := float64(i+1) / float64(blockCount) * 95
		up(progress)
	}

	log.Infof("[xiaomi_cloud] 所有分块上传完成,提交最终确认...")

	// 提交上传完成
	commitURL := fmt.Sprintf("%s%s/files/commit", BaseURL, APIPrefix)
	commitMetasJSON, _ := json.Marshal(commitMetas)

	var commitResp CreateUploadResp
	err = d.retryRequest(3, func() error {
		commitResp = CreateUploadResp{}
		_, requestErr := d.request(commitURL, http.MethodPost, func(req *resty.Request, serviceToken string) {
			req.SetHeader("Content-Type", "application/x-www-form-urlencoded")
			req.SetBody(buildFormData(map[string]interface{}{
				"uploadId":                uploadID,
				"file_meta":               fileMeta,
				"commit_metas":            string(commitMetasJSON),
				"parentId":                dstDir.GetID(),
				"temp":                    false,
				"isClientUploadThumbnail": false,
				"serviceToken":            serviceToken,
			}))
		}, &commitResp)
		return requestErr
	})

	if err != nil {
		return fmt.Errorf("提交上传失败: %w", err)
	}

	if commitResp.Code != 0 {
		return fmt.Errorf("提交上传失败: %s", commitResp.Description)
	}

	log.Infof("[xiaomi_cloud] 文件上传成功: %s (ID: %s)", fileName, commitResp.Data.ID)
	up(100)
	return nil
}

func (d *XiaomiCloud) GetDetails(ctx context.Context) (*model.StorageDetails, error) {
	apiURL := fmt.Sprintf("%s/status/alldetail", BaseURL)

	var resp StorageInfoResp
	_, err := d.request(apiURL, http.MethodGet, nil, &resp)
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

func buildKSSUploadURL(nodeURL, fileMeta, blockMeta string) string {
	query := url.Values{}
	// block_meta 已经标识文件块；chunk_pos 是该块内部的分片偏移。
	// 当前每个4 MiB文件块都通过一次请求完整上传，因此固定为0。
	query.Set("chunk_pos", "0")
	query.Set("file_meta", fileMeta)
	query.Set("block_meta", blockMeta)
	return fmt.Sprintf("%s/upload_block_chunk?%s", strings.TrimRight(nodeURL, "/"), query.Encode())
}
