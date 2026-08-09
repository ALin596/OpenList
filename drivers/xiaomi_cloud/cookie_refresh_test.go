package xiaomi_cloud

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestUpdateCookieFromResponse(t *testing.T) {
	tests := []struct {
		name          string
		initialToken  string
		initialUserID string
		initialSHash  string
		initialUHash  string
		setCookies    []string
		expectToken   string
		expectUserID  string
		expectSHash   string
		expectUHash   string
	}{
		{
			name:          "更新serviceToken和session_hash",
			initialToken:  "oldServiceToken123",
			initialUserID: "1234567890",
			initialSHash:  "oldSessionHash",
			initialUHash:  "oldUserHash",
			setCookies: []string{
				"serviceToken=newServiceToken456; Domain=.i.mi.com; Path=/",
				"i.mi.com_slh=newSessionHash; Domain=.i.mi.com; Path=/",
			},
			expectToken:  "newServiceToken456",
			expectUserID: "1234567890",
			expectSHash:  "newSessionHash",
			expectUHash:  "oldUserHash",
		},
		{
			name:          "所有字段都更新",
			initialToken:  "oldServiceToken123",
			initialUserID: "1234567890",
			initialSHash:  "oldSessionHash",
			initialUHash:  "oldUserHash",
			setCookies: []string{
				"serviceToken=newServiceToken789; Domain=.i.mi.com; Path=/",
				"userId=9999999999; Domain=.mi.com; Path=/",
				"i.mi.com_slh=newSessionHash2; Domain=.i.mi.com; Path=/",
				"i.mi.com_ph=newUserHash; Domain=.i.mi.com; Path=/",
			},
			expectToken:  "newServiceToken789",
			expectUserID: "9999999999",
			expectSHash:  "newSessionHash2",
			expectUHash:  "newUserHash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, cookie := range tt.setCookies {
					w.Header().Add("Set-Cookie", cookie)
				}
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"result":"ok","code":0}`))
			}))
			defer server.Close()

			driver := &XiaomiCloud{
				Addition: Addition{
					ServiceToken: tt.initialToken,
					UserID:       tt.initialUserID,
					SessionHash:  tt.initialSHash,
					UserHash:     tt.initialUHash,
				},
			}

			client := resty.New()
			resp, err := client.R().Get(server.URL)
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}

			driver.updateCookieFromResponse(resp)

			if driver.ServiceToken != tt.expectToken {
				t.Errorf("ServiceToken = %v, want %v", driver.ServiceToken, tt.expectToken)
			}
			if driver.UserID != tt.expectUserID {
				t.Errorf("UserID = %v, want %v", driver.UserID, tt.expectUserID)
			}
			if driver.SessionHash != tt.expectSHash {
				t.Errorf("SessionHash = %v, want %v", driver.SessionHash, tt.expectSHash)
			}
			if driver.UserHash != tt.expectUHash {
				t.Errorf("UserHash = %v, want %v", driver.UserHash, tt.expectUHash)
			}
		})
	}
}
