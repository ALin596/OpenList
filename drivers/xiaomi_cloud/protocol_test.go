package xiaomi_cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
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
	var resp uploadBlockResponse
	if err := driver.request(context.Background(), server.URL, http.MethodPost, nil, &resp); err != nil {
		t.Fatalf("request without JSON Content-Type: %v", err)
	}
	if resp.Stat != "BLOCK_COMPLETED" || resp.CommitMeta != "commit-1" {
		t.Fatalf("upload block response = %+v", resp)
	}
}

func TestRequestValidatesJSONWithoutResponseTarget(t *testing.T) {
	previousClient := base.RestyClient
	base.RestyClient = resty.New()
	t.Cleanup(func() {
		base.RestyClient = previousClient
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()

	err := (&XiaomiCloud{}).request(context.Background(), server.URL, http.MethodPost, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "解析响应失败") {
		t.Fatalf("request malformed response error = %v", err)
	}
}

func TestIsRetriableError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "gateway timeout", err: &httpStatusError{code: http.StatusGatewayTimeout}, want: true},
		{name: "bad gateway", err: &httpStatusError{code: http.StatusBadGateway}, want: true},
		{name: "service unavailable", err: &httpStatusError{code: http.StatusServiceUnavailable}, want: true},
		{name: "connection reset", err: errors.New("connection reset by peer"), want: true},
		{name: "retriable api error", err: &apiError{code: 1, retriable: true}, want: true},
		{name: "non-retriable api error", err: &apiError{code: 1}, want: false},
		{name: "canceled", err: context.Canceled, want: false},
		{name: "unauthorized", err: &httpStatusError{code: http.StatusUnauthorized}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetriableError(tt.err); got != tt.want {
				t.Fatalf("isRetriableError(%q) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNormalizeErrorUsesTypedCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{name: "API unauthorized", err: &apiError{code: http.StatusUnauthorized}, want: errs.EmptyToken},
		{name: "HTTP not found", err: &httpStatusError{code: http.StatusNotFound}, want: errs.ObjectNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := normalizeError(tt.err); !errors.Is(err, tt.want) {
				t.Fatalf("normalizeError(%v) = %v, want errors.Is(_, %v)", tt.err, err, tt.want)
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

func TestCreateUploadResponseUnmarshalChunkedUpload(t *testing.T) {
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

	var resp createUploadResponse
	if err := json.Unmarshal([]byte(fixture), &resp); err != nil {
		t.Fatalf("unmarshal chunked upload response: %v", err)
	}

	if resp.Data.Exists {
		t.Fatal("chunked upload response unexpectedly marked as rapid upload")
	}
	if resp.Data.KSS.FileMeta != "file-meta" {
		t.Fatalf("file_meta = %q, want %q", resp.Data.KSS.FileMeta, "file-meta")
	}
	if len(resp.Data.KSS.NodeURLs) != 1 {
		t.Fatalf("node_urls length = %d, want 1", len(resp.Data.KSS.NodeURLs))
	}
	if len(resp.Data.KSS.BlockMetas) != 2 {
		t.Fatalf("block_metas length = %d, want 2", len(resp.Data.KSS.BlockMetas))
	}
	if got := resp.Data.KSS.BlockMetas[1]; got.BlockMeta != "block-2" {
		t.Fatalf("second block meta = %+v, want block-2", got)
	}
}

func TestCreateUploadResponseUnmarshalRapidUpload(t *testing.T) {
	fixture := `{
		"result":"ok",
		"retriable":false,
		"code":0,
		"description":"success",
		"ts":1,
		"data":{"exists":true,"id":"file-1","uploadId":"upload-1"}
	}`

	var resp createUploadResponse
	if err := json.Unmarshal([]byte(fixture), &resp); err != nil {
		t.Fatalf("unmarshal rapid upload response: %v", err)
	}
	if !resp.Data.Exists || resp.Data.ID != "file-1" {
		t.Fatalf("rapid upload data = %+v", resp.Data)
	}
}

func TestFileInfoResponseUnmarshalNumericParentID(t *testing.T) {
	var resp fileInfoResponse
	if err := json.Unmarshal([]byte(`{"data":{"id":"file-1","parentId":0,"name":"root-file"}}`), &resp); err != nil {
		t.Fatalf("unmarshal file info response: %v", err)
	}
	if resp.Data.ID != "file-1" || string(resp.Data.ParentID) != "0" {
		t.Fatalf("file info response = %+v", resp.Data)
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
	moveRecord, err := json.Marshal(fileManagerRecord{ID: "file-1", ParentID: "folder-1", Type: "other"})
	if err != nil {
		t.Fatalf("marshal move record: %v", err)
	}
	if string(moveRecord) != `{"id":"file-1","parentId":"folder-1","type":"other"}` {
		t.Fatalf("move record JSON = %s", moveRecord)
	}

	deleteRecord, err := json.Marshal(fileManagerRecord{ID: "file-1", Type: "other"})
	if err != nil {
		t.Fatalf("marshal delete record: %v", err)
	}
	if string(deleteRecord) != `{"id":"file-1","type":"other"}` {
		t.Fatalf("delete record JSON = %s", deleteRecord)
	}
}

func TestMakeDirReturnsCreatedObjectAndBridgesVisibility(t *testing.T) {
	previousClient := base.RestyClient
	base.RestyClient = resty.New().SetRetryCount(0).SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case apiPrefix + "/folders/create":
			form, err := readRequestForm(req)
			if err != nil {
				return nil, err
			}
			if form.Get("name") != "created" || form.Get("parentId") != "parent-1" || form.Get("serviceToken") != "service" {
				return nil, errors.New("unexpected create-folder form")
			}
			return testResponse(req, http.StatusOK, `{
				"result":"ok","code":0,"data":{"id":"folder-1"}
			}`, nil), nil
		case apiPrefix + "/folders/children":
			return testResponse(req, http.StatusOK, `{
				"result":"ok","code":0,"data":{"records":[],"hasMore":false}
			}`, nil), nil
		default:
			return nil, errors.New("unexpected request")
		}
	}))
	t.Cleanup(func() {
		base.RestyClient = previousClient
	})

	driver := &XiaomiCloud{}
	setTestSession(driver, sessionCookies{serviceToken: "service", userID: "user", sessionHash: "session", userHash: "hash"})
	parent := &model.Object{ID: "parent-1", IsFolder: true}
	created, err := driver.MakeDir(context.Background(), parent, "created")
	if err != nil {
		t.Fatalf("MakeDir() error = %v", err)
	}
	if created.GetID() != "folder-1" || created.GetName() != "created" || !created.IsDir() {
		t.Fatalf("MakeDir() object = id:%q name:%q dir:%v", created.GetID(), created.GetName(), created.IsDir())
	}

	files, err := driver.List(context.Background(), parent, model.ListArgs{})
	if err != nil || len(files) != 1 || files[0].GetID() != "folder-1" {
		t.Fatalf("List() after MakeDir = %+v, %v", files, err)
	}
}
