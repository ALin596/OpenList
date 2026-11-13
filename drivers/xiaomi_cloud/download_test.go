package xiaomi_cloud

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/go-resty/resty/v2"
)

func TestLinkUsesJSONPAndPOSTFormDownload(t *testing.T) {
	downloadMeta := strings.Repeat("signed-meta", 7_000)
	tests := []struct {
		name       string
		status     int
		content    string
		contentRng string
	}{
		{name: "range response", status: http.StatusPartialContent, content: "cde", contentRng: "bytes 2-4/6"},
		{name: "range ignored", status: http.StatusOK, content: "abcdef"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previousClient := base.RestyClient
			var expectedCallback string
			base.RestyClient = resty.New().SetRetryCount(0).SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.URL.Hostname() == "i.mi.com" && req.URL.Path == apiPrefix+"/files/download":
					if req.Method != http.MethodGet || req.URL.Query().Get("ids") != `["file-1"]` {
						return nil, fmt.Errorf("下载信息请求不符合小米接口格式: %s %s", req.Method, req.URL)
					}
					expectedCallback = req.URL.Query().Get("jsonpCallback")
					if expectedCallback == "" {
						return nil, fmt.Errorf("下载信息请求缺少 JSONP callback")
					}
					body := fmt.Sprintf(`{"code":0,"data":{"downLoads":[{"jsonpUrl":"https://i.mi.com/jsonp","expireAt":%d}]}}`, time.Now().Add(time.Hour).UnixMilli())
					return testResponse(req, http.StatusOK, body, nil), nil
				case req.URL.Hostname() == "i.mi.com" && req.URL.Path == "/jsonp":
					callback := req.URL.Query().Get("callback")
					if callback != expectedCallback || req.URL.Query().Has("ts") {
						return nil, fmt.Errorf("JSONP 请求参数与网页端不符")
					}
					body := fmt.Sprintf(`%s({"url":"https://download.example.test/download_file?attachment=1&sig=short","meta":%q});`, callback, downloadMeta)
					return testResponse(req, http.StatusOK, body, nil), nil
				case req.URL.Hostname() == "download.example.test":
					if req.Method != http.MethodPost {
						return nil, fmt.Errorf("下载请求方法 = %s, want POST", req.Method)
					}
					if req.URL.Query().Has("meta") {
						return nil, fmt.Errorf("meta 被放入下载 URL 查询参数")
					}
					if req.Header.Get("Cookie") != "" {
						return nil, fmt.Errorf("下载 CDN 请求不应携带小米会话 Cookie")
					}
					if got := req.Header.Get("Range"); got != "bytes=2-4" {
						return nil, fmt.Errorf("下载 Range = %q, want bytes=2-4", got)
					}
					if err := req.ParseForm(); err != nil {
						return nil, fmt.Errorf("解析下载表单: %w", err)
					}
					if got := req.PostForm.Get("meta"); got != downloadMeta {
						return nil, fmt.Errorf("POST meta length = %d, want %d", len(got), len(downloadMeta))
					}
					header := make(http.Header)
					if tt.contentRng != "" {
						header.Set("Content-Range", tt.contentRng)
					}
					return testResponse(req, tt.status, tt.content, header), nil
				default:
					return nil, fmt.Errorf("unexpected Xiaomi download request: %s %s", req.Method, req.URL)
				}
			}))
			t.Cleanup(func() { base.RestyClient = previousClient })
			driver := &XiaomiCloud{}
			setTestSession(driver, sessionCookies{serviceToken: "service", userID: "user", sessionHash: "session", userHash: "hash"})

			link, err := driver.Link(context.Background(), &model.Object{ID: "file-1", Size: 6}, model.LinkArgs{})
			if err != nil {
				t.Fatalf("Link() error = %v", err)
			}
			if link.URL != "" || link.RangeReader == nil || link.ContentLength != 6 {
				t.Fatalf("Link() = URL %q, RangeReader %v, size %d", link.URL, link.RangeReader != nil, link.ContentLength)
			}

			body, err := link.RangeReader.RangeRead(context.Background(), http_range.Range{Start: 2, Length: 3})
			if err != nil {
				t.Fatalf("RangeRead() error = %v", err)
			}
			defer body.Close()
			data, err := io.ReadAll(body)
			if err != nil {
				t.Fatalf("read ranged body: %v", err)
			}
			if got := string(data); got != "cde" {
				t.Fatalf("RangeRead() = %q, want cde", got)
			}
		})
	}
}

func TestDecodeJSONPRejectsUnexpectedCallback(t *testing.T) {
	var response xiaomiDownload
	err := decodeJSONP("expected", []byte(`other({"url":"https://example.test"});`), &response)
	if err == nil || !strings.Contains(err.Error(), "回调") {
		t.Fatalf("decodeJSONP() error = %v, want callback mismatch", err)
	}
}

func TestLinkDownloadExpired(t *testing.T) {
	previousClient := base.RestyClient
	base.RestyClient = resty.New().SetRetryCount(0).SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		callback := req.URL.Query().Get("jsonpCallback")
		body := fmt.Sprintf(`{"code":0,"data":{"downLoads":[{"jsonpUrl":"https://i.mi.com/jsonp?callback=%s","expireAt":%d}]}}`, callback, time.Now().Add(-time.Minute).UnixMilli())
		return testResponse(req, http.StatusOK, body, nil), nil
	}))
	t.Cleanup(func() { base.RestyClient = previousClient })
	driver := &XiaomiCloud{}
	setTestSession(driver, sessionCookies{serviceToken: "service", userID: "user", sessionHash: "session", userHash: "hash"})
	if _, err := driver.Link(context.Background(), &model.Object{ID: "file-1", Size: 6}, model.LinkArgs{}); err == nil {
		t.Fatal("Link() succeeded with expired download information")
	}
}

func TestXiaomiCloudRequiresProxyForPostDownloads(t *testing.T) {
	if !(&XiaomiCloud{}).Config().NoLinkURL {
		t.Fatal("Xiaomi Cloud downloads require the OpenList proxy to submit POST forms")
	}
}
