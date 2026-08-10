package xiaomi_cloud

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	streamPkg "github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/go-resty/resty/v2"
)

func TestUploadHasherBuildsOrderedBlockManifest(t *testing.T) {
	data := []byte("abcdefghij")
	hasher := newUploadHasher(4)
	for _, part := range [][]byte{data[:3], data[3:8], data[8:]} {
		if _, err := hasher.Write(part); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	manifest, err := hasher.manifest(int64(len(data)))
	if err != nil {
		t.Fatalf("manifest() error = %v", err)
	}
	if manifest.fileSHA1 != hashHex(sha1.New(), data) {
		t.Fatalf("file SHA1 = %q", manifest.fileSHA1)
	}
	wantBlocks := [][]byte{data[:4], data[4:8], data[8:]}
	if len(manifest.blocks) != len(wantBlocks) {
		t.Fatalf("block count = %d, want %d", len(manifest.blocks), len(wantBlocks))
	}
	for index, block := range wantBlocks {
		got := manifest.blocks[index]
		if got.Size != int64(len(block)) || got.SHA1 != hashHex(sha1.New(), block) || got.MD5 != hashHex(md5.New(), block) {
			t.Fatalf("block %d = %+v", index, got)
		}
	}
}

func TestPutUploadsBlocksConcurrentlyAndCommitsInOrder(t *testing.T) {
	data := make([]byte, 2*chunkSize+321)
	for index := range data {
		data[index] = byte(index % 251)
	}

	var active atomic.Int32
	var maxActive atomic.Int32
	var commitMu sync.Mutex
	var committed []commitMeta
	previousClient := base.RestyClient
	base.RestyClient = resty.New().SetRetryCount(0).SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case apiPrefix + "/files/create":
			return testResponse(req, http.StatusOK, `{
				"result":"ok","code":0,"data":{
					"uploadId":"upload-1","exists":false,
					"kss":{"file_meta":"file-meta","node_urls":["https://upload.example.test/node"],
					"block_metas":[{"block_meta":"block-0"},{"block_meta":"block-1"},{"block_meta":"block-2"}]}
				}
			}`, nil), nil
		case "/node/upload_block_chunk":
			current := active.Add(1)
			for {
				maximum := maxActive.Load()
				if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
					break
				}
			}
			defer active.Add(-1)

			blockIndex, err := strconv.Atoi(req.URL.Query().Get("block_meta")[len("block-"):])
			if err != nil {
				return nil, err
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			offset := int64(blockIndex) * chunkSize
			end := min(offset+chunkSize, int64(len(data)))
			if !bytes.Equal(body, data[offset:end]) {
				return nil, fmt.Errorf("block %d body mismatch", blockIndex)
			}
			if req.Header.Get("Content-Length") != strconv.Itoa(len(body)) {
				return nil, fmt.Errorf("block %d Content-Length mismatch", blockIndex)
			}
			time.Sleep(40 * time.Millisecond)
			return testResponse(req, http.StatusOK,
				fmt.Sprintf(`{"commit_meta":"commit-%d","stat":"BLOCK_COMPLETED"}`, blockIndex), nil), nil
		case apiPrefix + "/files/commit":
			form, err := readRequestForm(req)
			if err != nil {
				return nil, err
			}
			var metas []commitMeta
			if err = json.Unmarshal([]byte(form.Get("commit_metas")), &metas); err != nil {
				return nil, err
			}
			commitMu.Lock()
			committed = metas
			commitMu.Unlock()
			return testResponse(req, http.StatusOK, `{"result":"ok","code":0,"data":{"id":"file-1"}}`, nil), nil
		default:
			return nil, fmt.Errorf("unexpected request: %s", req.URL)
		}
	}))
	t.Cleanup(func() {
		base.RestyClient = previousClient
	})

	driver := &XiaomiCloud{Addition: Addition{UploadThread: 2}}
	setTestSession(driver, sessionCookies{serviceToken: "service", userID: "user", sessionHash: "session", userHash: "hash"})
	stream := &streamPkg.FileStream{
		Obj:      &model.Object{Name: "large.bin", Size: int64(len(data))},
		Reader:   bytes.NewReader(data),
		Mimetype: "application/octet-stream",
	}
	if err := driver.Put(context.Background(), &model.Object{ID: "parent-1", IsFolder: true}, stream, func(float64) {}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if maxActive.Load() < 2 {
		t.Fatalf("max concurrent uploads = %d, want at least 2", maxActive.Load())
	}

	commitMu.Lock()
	defer commitMu.Unlock()
	if len(committed) != 3 {
		t.Fatalf("commit meta count = %d, want 3", len(committed))
	}
	for index, meta := range committed {
		if meta.CommitMeta != fmt.Sprintf("commit-%d", index) {
			t.Fatalf("commit meta %d = %q", index, meta.CommitMeta)
		}
	}
}

func TestRequestHonorsCanceledContext(t *testing.T) {
	previousClient := base.RestyClient
	base.RestyClient = resty.New().SetRetryCount(0).SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	t.Cleanup(func() {
		base.RestyClient = previousClient
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	driver := &XiaomiCloud{}
	err := driver.request(ctx, "https://upload.example.test/node", http.MethodPost, nil, nil)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("request() error = %v, want context.Canceled", err)
	}
}

func TestUploadBlockRetryRestartsSectionReader(t *testing.T) {
	data := []byte("retry-block")
	var attempts atomic.Int32
	previousClient := base.RestyClient
	base.RestyClient = resty.New().SetRetryCount(0).SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(body, data) {
			return nil, fmt.Errorf("retry body = %q, want %q", body, data)
		}
		if attempts.Add(1) == 1 {
			return testResponse(req, http.StatusServiceUnavailable, "unavailable", nil), nil
		}
		return testResponse(req, http.StatusOK, `{"commit_meta":"commit-0","stat":"BLOCK_COMPLETED"}`, nil), nil
	}))
	t.Cleanup(func() {
		base.RestyClient = previousClient
	})

	driver := &XiaomiCloud{Addition: Addition{UploadThread: 1}}
	manifest := uploadManifest{
		fileSize: int64(len(data)),
		blocks:   []blockInfo{{Size: int64(len(data))}},
	}
	session := uploadSession{
		fileMeta:   "file-meta",
		nodeURLs:   []string{"https://upload.example.test/node"},
		blockMetas: []uploadBlockMeta{{BlockMeta: "block-0"}},
	}
	metas, err := driver.uploadBlocks(context.Background(), bytes.NewReader(data), manifest, session, func(float64) {})
	if err != nil {
		t.Fatalf("uploadBlocks() error = %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("upload attempts = %d, want 2", attempts.Load())
	}
	if len(metas) != 1 || metas[0].CommitMeta != "commit-0" {
		t.Fatalf("commit metas = %+v", metas)
	}
}

func TestNormalizeUploadThreads(t *testing.T) {
	for _, test := range []struct {
		input int
		want  int
	}{{0, defaultUploadThreads}, {-1, defaultUploadThreads}, {1, 1}, {8, 8}, {99, maxUploadThreads}} {
		if got := normalizeUploadThreads(test.input); got != test.want {
			t.Fatalf("normalizeUploadThreads(%d) = %d, want %d", test.input, got, test.want)
		}
	}
}

func hashHex(hasher interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}, data []byte) string {
	_, _ = hasher.Write(data)
	return hex.EncodeToString(hasher.Sum(nil))
}

func readRequestForm(req *http.Request) (url.Values, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	return url.ParseQuery(string(body))
}
