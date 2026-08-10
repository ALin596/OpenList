package xiaomi_cloud

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/go-resty/resty/v2"
)

func TestCookieHeaderHelpers(t *testing.T) {
	cookies := parseCookieHeader("passToken=token+/=; userId=123456; malformed")
	if !containsCookie(cookies, "passToken") || !containsCookie(cookies, "userId") {
		t.Fatalf("failed to parse account cookies: %+v", cookies)
	}
	if containsCookie(cookies, "malformed") {
		t.Fatal("malformed cookie was unexpectedly accepted")
	}

	header := cookieHeader(cookies)
	if !strings.Contains(header, "passToken=token+/=") || !strings.Contains(header, "userId=123456") {
		t.Fatalf("unexpected serialized cookie header: %s", header)
	}
}

func TestSessionCookiesApplyIgnoresDeletion(t *testing.T) {
	session := sessionCookies{serviceToken: "old-service"}
	updated := session.apply(parseCookieHeader("serviceToken=; userId=123456; i.mi.com_slh=session; i.mi.com_ph=user"))
	if !updated {
		t.Fatal("session was not updated")
	}
	if session.serviceToken != "old-service" {
		t.Fatalf("serviceToken deletion replaced in-memory token: %q", session.serviceToken)
	}
	if !session.complete() {
		t.Fatalf("session should be complete: %+v", session)
	}
}

func TestRequestDoesNotSendSessionCookiesToOtherHosts(t *testing.T) {
	previousClient := base.RestyClient
	base.RestyClient = resty.New().SetRetryCount(0).SetTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if cookie := req.Header.Get("Cookie"); cookie != "" {
			t.Fatalf("unexpected Cookie header for %s: %s", req.URL.Hostname(), cookie)
		}
		return testResponse(req, http.StatusOK, `{"commit_meta":"commit-1","stat":"BLOCK_COMPLETED"}`, nil), nil
	}))
	t.Cleanup(func() {
		base.RestyClient = previousClient
	})

	driver := &XiaomiCloud{}
	driver.setSession(sessionCookies{serviceToken: "service", userID: "user", sessionHash: "session", userHash: "hash"})
	var resp UploadBlockResp
	if _, err := driver.request(context.Background(), "https://upload.example.test/node", http.MethodPost, nil, &resp); err != nil {
		t.Fatalf("request() error = %v", err)
	}
}
