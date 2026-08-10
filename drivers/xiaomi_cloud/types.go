package xiaomi_cloud

import (
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

type file struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	CreateTime int64  `json:"createTime"`
	ModifyTime int64  `json:"modifyTime"`
	SHA1       string `json:"sha1"`
}

func fileToObj(f file) model.Obj {
	return &model.Object{
		ID:       f.ID,
		Name:     f.Name,
		Size:     f.Size,
		Modified: time.UnixMilli(f.ModifyTime),
		Ctime:    time.UnixMilli(f.CreateTime),
		IsFolder: f.Type == "folder",
		HashInfo: utils.NewHashInfo(utils.SHA1, f.SHA1),
	}
}

type listResponse struct {
	Data struct {
		Records []file `json:"records"`
		HasMore bool   `json:"hasMore"`
	} `json:"data"`
}

type fileInfoResponse struct {
	Data struct {
		Storage struct {
			DownloadURL string `json:"downloadUrl"`
			ExpireAt    int64  `json:"expireAt"`
		} `json:"storage"`
	} `json:"data"`
}

type storageInfoResponse struct {
	Data struct {
		TotalQuota int64 `json:"totalQuota"`
		Used       int64 `json:"used"`
	} `json:"data"`
}

type createUploadResponse struct {
	Data struct {
		UploadID string `json:"uploadId"`
		Exists   bool   `json:"exists"`
		ID       string `json:"id"`
		KSS      struct {
			BlockMetas []uploadBlockMeta `json:"block_metas,omitempty"`
			FileMeta   string            `json:"file_meta,omitempty"`
			NodeURLs   []string          `json:"node_urls,omitempty"`
		} `json:"kss,omitempty"`
	} `json:"data"`
}

type uploadBlockMeta struct {
	BlockMeta string `json:"block_meta"`
}

type uploadBlockResponse struct {
	CommitMeta string `json:"commit_meta"`
	Stat       string `json:"stat"`
}

type commitMeta struct {
	CommitMeta string `json:"commit_meta"`
}

type blockInfo struct {
	SHA1 string `json:"sha1"`
	MD5  string `json:"md5"`
	Size int64  `json:"size"`
}

type kssInfo struct {
	BlockInfos []blockInfo `json:"block_infos"`
	SplitSize  int64       `json:"splitSize"`
}

type uploadFileData struct {
	FileName        string         `json:"fileName"`
	Size            int64          `json:"size"`
	SHA1            string         `json:"sha1"`
	MIMEType        string         `json:"mimeType"`
	Channel         string         `json:"channel"`
	ParentID        string         `json:"parentId"`
	LocalCreateTime int64          `json:"localCreateTime"`
	LocalModifyTime int64          `json:"localModifyTime"`
	ExifInfo        map[string]any `json:"exifInfo"`
}

type createFolderResponse struct {
	Data file `json:"data"`
}

type fileManagerResponse struct {
	Data struct {
		TaskID string `json:"taskId"`
	} `json:"data"`
}

type fileManagerRecord struct {
	ID       string `json:"id"`
	ParentID string `json:"parentId,omitempty"`
	Type     string `json:"type"`
}

type taskQueryResponse struct {
	Data struct {
		Status string `json:"status"`
	} `json:"data"`
}

type errorResponse struct {
	Retriable   bool   `json:"retriable"`
	Code        int    `json:"code"`
	Description string `json:"description"`
	Message     string `json:"message"`
}

func (r errorResponse) description() string {
	if r.Description != "" {
		return r.Description
	}
	if r.Message != "" {
		return r.Message
	}
	return "unknown error"
}
