package xiaomi_cloud

import (
	"strconv"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

// File 小米云盘文件/文件夹对象
type File struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Type            string      `json:"type"` // "folder" 或 "other"
	Size            int64       `json:"size"`
	ParentID        interface{} `json:"parentId"`   // 可能是数字0或字符串
	CreateTime      int64       `json:"createTime"` // 毫秒时间戳
	ModifyTime      int64       `json:"modifyTime"` // 毫秒时间戳
	Revision        string      `json:"revision"`
	Sha1            string      `json:"sha1"`  // 仅文件有
	Label           string      `json:"label"` // 文件扩展名
	Status          string      `json:"status"`
	PrivacyStatus   string      `json:"privacyStatus"`
	ShowStatus      string      `json:"showStatus"`
	Channel         string      `json:"channel"` // 上传渠道
	AppID           string      `json:"appId"`
	LocalCreateTime int64       `json:"localCreateTime,omitempty"`
	LocalModifyTime int64       `json:"localModifyTime,omitempty"`
}

// fileToObj 转换小米云盘文件对象为OpenList对象
func fileToObj(f File) model.Obj {
	return &model.Object{
		ID:       f.ID,
		Name:     f.Name,
		Size:     f.Size,
		Modified: time.UnixMilli(f.ModifyTime),
		Ctime:    time.UnixMilli(f.CreateTime),
		IsFolder: f.Type == "folder",
		HashInfo: utils.NewHashInfo(utils.SHA1, f.Sha1),
	}
}

// ListResp 文件列表响应
type ListResp struct {
	Result      string `json:"result"`
	Retriable   bool   `json:"retriable"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	TS          int64  `json:"ts"`
	Data        struct {
		Records  []File `json:"records"`
		HasMore  bool   `json:"hasMore"`
		AllCount int    `json:"allCount"`
	} `json:"data"`
}

// FileInfoResp 文件详情响应
type FileInfoResp struct {
	Result      string `json:"result"`
	Retriable   bool   `json:"retriable"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	TS          int64  `json:"ts"`
	Data        struct {
		ID         string      `json:"id"`
		Name       string      `json:"name"`
		Type       string      `json:"type"`
		Size       int64       `json:"size"`
		Sha1       string      `json:"sha1"`
		ParentID   interface{} `json:"parentId"` // 可能是数字0或字符串
		CreateTime int64       `json:"createTime"`
		ModifyTime int64       `json:"modifyTime"`
		Storage    struct {
			Kss struct {
				Blocks []struct {
					IDCName string   `json:"idc_name"`
					Sha1    string   `json:"sha1"`
					Size    int64    `json:"size"`
					URLs    []string `json:"urls"`
				} `json:"blocks"`
				SecureKey string `json:"secure_key"`
				Stat      string `json:"stat"`
			} `json:"kss"`
			DownloadURL string `json:"downloadUrl"`
			ExpireAt    int64  `json:"expireAt"`
		} `json:"storage"`
	} `json:"data"`
}

// StorageInfoResp 存储空间响应
type StorageInfoResp struct {
	Result      string `json:"result"`
	Retriable   bool   `json:"retriable"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	TS          int64  `json:"ts"`
	Data        struct {
		TotalQuota  int64 `json:"totalQuota"`
		Used        int64 `json:"used"`
		TotalDetail struct {
			BasicQuota              int64  `json:"basicQuota"`
			YearlyPackageSize       int64  `json:"yearlyPackageSize"`
			YearlyPackageType       string `json:"yearlyPackageType"`
			YearlyPackageExpireTime int64  `json:"yearlyPackageExpireTime"`
			ExtendPackageSize       int64  `json:"extendPackageSize"`
			BonusSize               int64  `json:"bonusSize"`
		} `json:"totalDetail"`
		UsedDetail map[string]struct {
			Size int64  `json:"size"`
			Text string `json:"text"`
		} `json:"usedDetail"`
	} `json:"data"`
}

// CreateUploadResp 创建上传任务响应
type CreateUploadResp struct {
	Result      string `json:"result"`
	Retriable   bool   `json:"retriable"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	TS          int64  `json:"ts"`
	Data        struct {
		UploadID string `json:"uploadId"`
		Exists   bool   `json:"exists"` // true表示秒传成功
		ID       string `json:"id"`     // 文件ID
		Kss      struct {
			BlockMetas      []UploadBlockMeta `json:"block_metas,omitempty"`
			ContentCacheKey string            `json:"contentCacheKey,omitempty"`
			FileMeta        string            `json:"file_meta,omitempty"`
			NodeURLs        []string          `json:"node_urls,omitempty"`
			Stat            string            `json:"stat,omitempty"`
		} `json:"kss,omitempty"`
	} `json:"data"`
}

// UploadBlockMeta 小米云盘为每个文件块返回的上传元数据。
// is_existed 在实际响应中是数字（0/1），不是布尔值。
type UploadBlockMeta struct {
	BlockMeta string `json:"block_meta"`
	IsExisted int    `json:"is_existed"`
}

// UploadBlockResp 单个KSS文件块上传响应
type UploadBlockResp struct {
	CommitMeta string `json:"commit_meta"`
	Stat       string `json:"stat"`
}

// CommitMeta 提交上传时的单块确认信息
type CommitMeta struct {
	CommitMeta string `json:"commit_meta"`
}

// BlockInfo 文件块信息(用于上传)
type BlockInfo struct {
	Sha1 string `json:"sha1"`
	MD5  string `json:"md5"`
	Size int64  `json:"size"`
}

// KssInfo 分块存储信息
type KssInfo struct {
	BlockInfos []BlockInfo `json:"block_infos"`
	SplitSize  int64       `json:"splitSize"` // 4194304 (4MB)
}

// UploadFileData 上传文件元数据
type UploadFileData struct {
	FileName        string                 `json:"fileName"`
	Size            int64                  `json:"size"`
	Sha1            string                 `json:"sha1"`
	MimeType        string                 `json:"mimeType"`
	Channel         string                 `json:"channel"`
	ParentID        string                 `json:"parentId"`
	LocalCreateTime int64                  `json:"localCreateTime"`
	LocalModifyTime int64                  `json:"localModifyTime"`
	ExifInfo        map[string]interface{} `json:"exifInfo"`
}

// CreateFolderResp 创建文件夹响应
type CreateFolderResp struct {
	Result      string `json:"result"`
	Retriable   bool   `json:"retriable"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	TS          int64  `json:"ts"`
	Data        File   `json:"data"`
}

// DeleteResp 删除操作响应(异步任务)
type DeleteResp struct {
	Result      string `json:"result"`
	Retriable   bool   `json:"retriable"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	TS          int64  `json:"ts"`
	Data        struct {
		TaskID     string `json:"taskId"`
		ExpireTime int64  `json:"expireTime"`
		Interval   int    `json:"interval"` // 轮询间隔(毫秒)
	} `json:"data"`
}

// FileManagerRecord 移动、删除等批量文件管理操作的单条记录
type FileManagerRecord struct {
	ID       string `json:"id"`
	ParentID string `json:"parentId,omitempty"`
	Type     string `json:"type"`
}

// TaskQueryResp 任务查询响应
type TaskQueryResp struct {
	Result      string `json:"result"`
	Retriable   bool   `json:"retriable"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	TS          int64  `json:"ts"`
	Data        struct {
		TaskID      string  `json:"taskId"`
		OperateType string  `json:"operateType"`
		Status      string  `json:"status"` // SUCCESS, PROCESSING, FAILED
		Progress    float64 `json:"progress,omitempty"`
	} `json:"data"`
}

// RenameResp 重命名响应
type RenameResp struct {
	Result      string `json:"result"`
	Retriable   bool   `json:"retriable"`
	Reason      string `json:"reason"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	TS          int64  `json:"ts"`
}

// SearchResp 搜索响应
type SearchResp struct {
	Result      string `json:"result"`
	Retriable   bool   `json:"retriable"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	TS          int64  `json:"ts"`
	Data        struct {
		Records  []File `json:"records"`
		HasMore  bool   `json:"hasMore"`
		AllCount int    `json:"allCount"`
	} `json:"data"`
}

// ErrorResp 错误响应
type ErrorResp struct {
	Result      string `json:"result"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	Message     string `json:"message"`
}

// getParentIDString 统一处理parentId字段(可能是数字0或字符串)
func getParentIDString(parentID interface{}) string {
	switch v := parentID.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	default:
		return "0"
	}
}
