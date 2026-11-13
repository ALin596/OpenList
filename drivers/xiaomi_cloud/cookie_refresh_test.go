package xiaomi_cloud

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/go-resty/resty/v2"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testResponse(req *http.Request, status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func setTestSession(driver *XiaomiCloud, session sessionCookies) {
	driver.sessionMu.Lock()
	driver.session = session
	driver.sessionMu.Unlock()
}

func installAuthFlow(t *testing.T, serviceLoginCount *atomic.Int32) {
	t.Helper()
	previousFactory := newXiaomiAuthClient
	newXiaomiAuthClient = func() *resty.Client {
		return resty.New().SetRetryCount(0).SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case req.URL.Hostname() == "i.mi.com" && req.URL.Path == "/api/user/login":
				return testResponse(req, http.StatusOK, `{"code":0,"data":{"loginUrl":"https://account.xiaomi.com/pass/serviceLogin?sid=i.mi.com"}}`, nil), nil
			case req.URL.Hostname() == "account.xiaomi.com" && req.URL.Path == "/pass/serviceLogin":
				serviceLoginCount.Add(1)
				cookie, err := req.Cookie("passToken")
				if err != nil || cookie.Value != "account-pass" {
					return nil, fmt.Errorf("serviceLogin missing account passToken")
				}
				header := make(http.Header)
				header.Set("Location", "https://i.mi.com/sts?ticket=test")
				header.Add("Set-Cookie", "passToken=renewed-pass; Domain=.account.xiaomi.com; Path=/; Secure; HttpOnly")
				return testResponse(req, http.StatusFound, "", header), nil
			case req.URL.Hostname() == "i.mi.com" && req.URL.Path == "/sts":
				header := make(http.Header)
				header.Set("Location", "https://i.mi.com/drive/h5")
				header.Add("Set-Cookie", "serviceToken=new-service; Domain=.i.mi.com; Path=/; Secure")
				header.Add("Set-Cookie", "userId=123456; Domain=.mi.com; Path=/")
				header.Add("Set-Cookie", "i.mi.com_slh=new-session-hash; Domain=.i.mi.com; Path=/")
				header.Add("Set-Cookie", "i.mi.com_ph=new-user-hash; Domain=.i.mi.com; Path=/")
				return testResponse(req, http.StatusFound, "", header), nil
			case req.URL.Hostname() == "i.mi.com" && req.URL.Path == "/drive/h5":
				return testResponse(req, http.StatusOK, "ok", nil), nil
			default:
				return nil, fmt.Errorf("unexpected auth request: %s", req.URL)
			}
		}))
	}
	t.Cleanup(func() {
		newXiaomiAuthClient = previousFactory
	})
}

func TestEnsureServiceTokenFromAccountCookie(t *testing.T) {
	var serviceLoginCount atomic.Int32
	installAuthFlow(t, &serviceLoginCount)

	driver := &XiaomiCloud{Addition: Addition{AccountCookie: "passToken=account-pass; userId=123456"}}
	if err := driver.ensureServiceToken(context.Background(), ""); err != nil {
		t.Fatalf("ensureServiceToken() error = %v", err)
	}

	session := driver.sessionSnapshot()
	if session.serviceToken != "new-service" || session.userID != "123456" || session.sessionHash != "new-session-hash" || session.userHash != "new-user-hash" {
		t.Fatalf("unexpected refreshed session: %+v", session)
	}
	if !hasCookie(driver.AccountCookie, "passToken") || !strings.Contains(driver.AccountCookie, "renewed-pass") {
		t.Fatalf("account cookie was not refreshed: %s", driver.AccountCookie)
	}
	if serviceLoginCount.Load() != 1 {
		t.Fatalf("serviceLogin count = %d, want 1", serviceLoginCount.Load())
	}
}

func TestDropClearsSessionAndForcesRefresh(t *testing.T) {
	var serviceLoginCount atomic.Int32
	installAuthFlow(t, &serviceLoginCount)

	driver := &XiaomiCloud{Addition: Addition{AccountCookie: "passToken=account-pass; userId=123456"}}
	setTestSession(driver, sessionCookies{serviceToken: "old-service", userID: "123456", sessionHash: "old-session", userHash: "old-user"})
	driver.rememberRecentFile("0", file{ID: "old-file", ParentID: flexibleID("0"), Name: "old.bin", Type: "other"})

	if err := driver.Drop(context.Background()); err != nil {
		t.Fatalf("Drop() error = %v", err)
	}

	header := make(http.Header)
	header.Add("Set-Cookie", "serviceToken=stale-service; Domain=.i.mi.com; Path=/; Secure")
	staleResponse := &resty.Response{RawResponse: testResponse(
		httptest.NewRequest(http.MethodGet, baseURL, nil), http.StatusOK, "{}", header,
	)}
	driver.updateSessionFromResponse(staleResponse, "old-service")
	if session := driver.sessionSnapshot(); session != (sessionCookies{}) {
		t.Fatalf("session after Drop() = %+v", session)
	}
	driver.recentFileMu.Lock()
	recentCount := len(driver.recentFiles)
	driver.recentFileMu.Unlock()
	if recentCount != 0 {
		t.Fatalf("recent file parents after Drop() = %d", recentCount)
	}

	if err := driver.ensureServiceToken(context.Background(), ""); err != nil {
		t.Fatalf("ensureServiceToken() after Drop error = %v", err)
	}
	if session := driver.sessionSnapshot(); session.serviceToken != "new-service" {
		t.Fatalf("refreshed service token = %q", session.serviceToken)
	}
	if serviceLoginCount.Load() != 1 {
		t.Fatalf("serviceLogin count = %d, want 1", serviceLoginCount.Load())
	}
}

func TestRequestRefreshesAndRebuildsPostBody(t *testing.T) {
	var serviceLoginCount atomic.Int32
	installAuthFlow(t, &serviceLoginCount)

	previousClient := base.RestyClient
	var attemptsMu sync.Mutex
	var attempts []string
	base.RestyClient = resty.New().SetRetryCount(0).SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		cookie, err := req.Cookie("serviceToken")
		if err != nil {
			return nil, err
		}
		attempt := cookie.Value + "/" + form.Get("serviceToken")
		attemptsMu.Lock()
		attempts = append(attempts, attempt)
		attemptsMu.Unlock()
		if cookie.Value == "old-service" {
			return testResponse(req, http.StatusUnauthorized, `{"R":401,"isSecondValidation":false}`, nil), nil
		}
		return testResponse(req, http.StatusOK, `{"result":"ok","code":0}`, nil), nil
	}))
	t.Cleanup(func() {
		base.RestyClient = previousClient
	})

	driver := &XiaomiCloud{Addition: Addition{AccountCookie: "passToken=account-pass; userId=123456"}}
	setTestSession(driver, sessionCookies{serviceToken: "old-service", userID: "123456", sessionHash: "old-session", userHash: "old-user"})

	var result struct {
		Code int `json:"code"`
	}
	err := driver.request(context.Background(), baseURL+"/drive/v2/user/test", http.MethodPost, func(req *resty.Request, serviceToken string) {
		req.SetFormData(map[string]string{"serviceToken": serviceToken})
	}, &result)
	if err != nil {
		t.Fatalf("request() error = %v", err)
	}
	if result.Code != 0 {
		t.Fatalf("response code = %d, want 0", result.Code)
	}
	if got, want := strings.Join(attempts, ","), "old-service/old-service,new-service/new-service"; got != want {
		t.Fatalf("attempt tokens = %q, want %q", got, want)
	}
	if serviceLoginCount.Load() != 1 {
		t.Fatalf("serviceLogin count = %d, want 1", serviceLoginCount.Load())
	}
}

func TestConcurrentUnauthorizedRequestsShareRefresh(t *testing.T) {
	var serviceLoginCount atomic.Int32
	installAuthFlow(t, &serviceLoginCount)

	previousClient := base.RestyClient
	var oldRequests atomic.Int32
	releaseUnauthorized := make(chan struct{})
	base.RestyClient = resty.New().SetRetryCount(0).SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		cookie, err := req.Cookie("serviceToken")
		if err != nil {
			return nil, err
		}
		if cookie.Value == "old-service" {
			if oldRequests.Add(1) == 2 {
				close(releaseUnauthorized)
			}
			<-releaseUnauthorized
			return testResponse(req, http.StatusUnauthorized, `{"R":401}`, nil), nil
		}
		return testResponse(req, http.StatusOK, `{"result":"ok","code":0}`, nil), nil
	}))
	t.Cleanup(func() {
		base.RestyClient = previousClient
	})

	driver := &XiaomiCloud{Addition: Addition{AccountCookie: "passToken=account-pass; userId=123456"}}
	setTestSession(driver, sessionCookies{serviceToken: "old-service", userID: "123456", sessionHash: "old-session", userHash: "old-user"})

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			var result struct {
				Code int `json:"code"`
			}
			err := driver.request(context.Background(), baseURL+"/drive/v2/user/test", http.MethodGet, nil, &result)
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent request error = %v", err)
		}
	}
	if serviceLoginCount.Load() != 1 {
		t.Fatalf("serviceLogin count = %d, want 1", serviceLoginCount.Load())
	}
}
