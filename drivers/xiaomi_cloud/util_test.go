package xiaomi_cloud

import (
	"context"
	"net/http"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/drivers/base"
	"github.com/go-resty/resty/v2"
)

func TestSessionCookiesApplyIgnoresDeletion(t *testing.T) {
	session := sessionCookies{serviceToken: "old-service"}
	session.apply([]*http.Cookie{
		{Name: "serviceToken", Value: ""},
		{Name: "userId", Value: "123456"},
		{Name: "i.mi.com_slh", Value: "session"},
		{Name: "i.mi.com_ph", Value: "user"},
	})
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
	setTestSession(driver, sessionCookies{serviceToken: "service", userID: "user", sessionHash: "session", userHash: "hash"})
	var resp uploadBlockResponse
	if err := driver.request(context.Background(), "https://upload.example.test/node", http.MethodPost, nil, &resp); err != nil {
		t.Fatalf("request() error = %v", err)
	}
}
