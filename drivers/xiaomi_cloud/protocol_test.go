package xiaomi_cloud

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/go-resty/resty/v2"
)

func TestRequestUnmarshalsJSONWithoutContentType(t *testing.T) {
	previousClient := base.RestyClient
	base.RestyClient = resty.New()
	t.Cleanup(func() {
		base.RestyClient = previousClient
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"commit_meta":"commit-1","stat":"BLOCK_COMPLETED"}`))
	}))
	defer server.Close()

	driver := &XiaomiCloud{}
	var resp UploadBlockResp
	if _, err := driver.request(server.URL, http.MethodPost, nil, &resp); err != nil {
		t.Fatalf("request without JSON Content-Type: %v", err)
	}
	if resp.Stat != "BLOCK_COMPLETED" || resp.CommitMeta != "commit-1" {
		t.Fatalf("upload block response = %+v", resp)
	}
}

func TestIsRetriableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "gateway timeout", err: errors.New("HTTP错误: 504 504 Gateway Timeout"), want: true},
		{name: "bad gateway", err: errors.New("HTTP错误: 502 502 Bad Gateway"), want: true},
		{name: "service unavailable", err: errors.New("HTTP错误: 503 503 Service Unavailable"), want: true},
		{name: "connection reset", err: errors.New("connection reset by peer"), want: true},
		{name: "unauthorized", err: errors.New("HTTP错误: 401 401 Unauthorized"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetriableError(tt.err); got != tt.want {
				t.Fatalf("isRetriableError(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestEvaluateTaskStatus(t *testing.T) {
	tests := []struct {
		status  string
		done    bool
		wantErr bool
	}{
		{status: "RUNNING", done: false},
		{status: "PROCESSING", done: false},
		{status: "SUCCESS", done: true},
		{status: "FAILED", wantErr: true},
		{status: "UNKNOWN", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			done, err := evaluateTaskStatus(tt.status, "task-1")
			if done != tt.done || (err != nil) != tt.wantErr {
				t.Fatalf("evaluateTaskStatus(%q) = (%v, %v), want done=%v, wantErr=%v", tt.status, done, err, tt.done, tt.wantErr)
			}
		})
	}
}

func TestCreateUploadRespUnmarshalChunkedUpload(t *testing.T) {
	fixture := `{
		"result":"ok",
		"retriable":false,
		"code":0,
		"description":"success",
		"ts":1,
		"data":{
			"exists":false,
			"uploadId":"upload-1",
			"kss":{
				"block_metas":[
					{"block_meta":"block-1","is_existed":0},
					{"block_meta":"block-2","is_existed":1}
				],
				"contentCacheKey":"cache-key",
				"file_meta":"file-meta",
				"node_urls":["https://upload.example.test/node"],
				"stat":"OK"
			}
		}
	}`

	var resp CreateUploadResp
	if err := json.Unmarshal([]byte(fixture), &resp); err != nil {
		t.Fatalf("unmarshal chunked upload response: %v", err)
	}

	if resp.Data.Exists {
		t.Fatal("chunked upload response unexpectedly marked as rapid upload")
	}
	if resp.Data.Kss.FileMeta != "file-meta" {
		t.Fatalf("file_meta = %q, want %q", resp.Data.Kss.FileMeta, "file-meta")
	}
	if len(resp.Data.Kss.NodeURLs) != 1 {
		t.Fatalf("node_urls length = %d, want 1", len(resp.Data.Kss.NodeURLs))
	}
	if len(resp.Data.Kss.BlockMetas) != 2 {
		t.Fatalf("block_metas length = %d, want 2", len(resp.Data.Kss.BlockMetas))
	}
	if got := resp.Data.Kss.BlockMetas[1]; got.BlockMeta != "block-2" || got.IsExisted != 1 {
		t.Fatalf("second block meta = %+v, want block-2 with is_existed=1", got)
	}
}

func TestCreateUploadRespUnmarshalRapidUpload(t *testing.T) {
	fixture := `{
		"result":"ok",
		"retriable":false,
		"code":0,
		"description":"success",
		"ts":1,
		"data":{"exists":true,"id":"file-1","uploadId":"upload-1"}
	}`

	var resp CreateUploadResp
	if err := json.Unmarshal([]byte(fixture), &resp); err != nil {
		t.Fatalf("unmarshal rapid upload response: %v", err)
	}
	if !resp.Data.Exists || resp.Data.ID != "file-1" {
		t.Fatalf("rapid upload data = %+v", resp.Data)
	}
}

func TestFileInfoRespUnmarshalKSSBlocks(t *testing.T) {
	fixture := `{
		"result":"ok",
		"code":0,
		"data":{
			"id":"file-1",
			"storage":{
				"downloadUrl":"https://download.example.test/file",
				"expireAt":1,
				"kss":{
					"blocks":[{"idc_name":"idc-1","sha1":"sha1","size":4194304,"urls":["https://download.example.test/block"]}],
					"secure_key":"secure-key",
					"stat":"OK"
				}
			}
		}
	}`

	var resp FileInfoResp
	if err := json.Unmarshal([]byte(fixture), &resp); err != nil {
		t.Fatalf("unmarshal file info response: %v", err)
	}
	if len(resp.Data.Storage.Kss.Blocks) != 1 {
		t.Fatalf("KSS blocks length = %d, want 1", len(resp.Data.Storage.Kss.Blocks))
	}
	block := resp.Data.Storage.Kss.Blocks[0]
	if block.IDCName != "idc-1" || len(block.URLs) != 1 {
		t.Fatalf("KSS block = %+v", block)
	}
}

func TestBuildKSSUploadURL(t *testing.T) {
	uploadURL := buildKSSUploadURL("https://upload.example.test/node/", "file meta", "block+meta")
	parsed, err := url.Parse(uploadURL)
	if err != nil {
		t.Fatalf("parse upload URL: %v", err)
	}

	query := parsed.Query()
	if got := query.Get("chunk_pos"); got != "0" {
		t.Fatalf("chunk_pos = %q, want 0", got)
	}
	if got := query.Get("file_meta"); got != "file meta" {
		t.Fatalf("file_meta = %q, want %q", got, "file meta")
	}
	if got := query.Get("block_meta"); got != "block+meta" {
		t.Fatalf("block_meta = %q, want %q", got, "block+meta")
	}
}

func TestGetFileManagerRecordType(t *testing.T) {
	tests := []struct {
		name string
		obj  model.Obj
		want string
	}{
		{name: "folder", obj: &model.Object{Name: "folder", IsFolder: true}, want: "folder"},
		{name: "picture", obj: &model.Object{Name: "photo.jpg"}, want: "pic"},
		{name: "other file", obj: &model.Object{Name: "archive.zip"}, want: "other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getFileManagerRecordType(tt.obj); got != tt.want {
				t.Fatalf("getFileManagerRecordType() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFileManagerRecordJSONShape(t *testing.T) {
	moveRecord, err := json.Marshal(FileManagerRecord{ID: "file-1", ParentID: "folder-1", Type: "other"})
	if err != nil {
		t.Fatalf("marshal move record: %v", err)
	}
	if string(moveRecord) != `{"id":"file-1","parentId":"folder-1","type":"other"}` {
		t.Fatalf("move record JSON = %s", moveRecord)
	}

	deleteRecord, err := json.Marshal(FileManagerRecord{ID: "file-1", Type: "other"})
	if err != nil {
		t.Fatalf("marshal delete record: %v", err)
	}
	if string(deleteRecord) != `{"id":"file-1","type":"other"}` {
		t.Fatalf("delete record JSON = %s", deleteRecord)
	}
}
