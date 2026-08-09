package xiaomi_cloud

import (
	"testing"
)

func TestParseCookieString(t *testing.T) {
	tests := []struct {
		name           string
		cookieStr      string
		wantErr        bool
		expectedToken  string
		expectedUserID string
		expectedSHash  string
		expectedUHash  string
	}{
		{
			name:           "完整的浏览器Cookie格式",
			cookieStr:      `userId=1234567890; i.mi.com_ph=test-user-hash==; i.mi.com_isvalid_servicetoken=true; i.mi.com_istrudev=true; csrfToken=test-csrf-token; serviceToken=test-service-token+/=; i.mi.com_slh=test-session-hash=`,
			wantErr:        false,
			expectedToken:  "test-service-token+/=",
			expectedUserID: "1234567890",
			expectedSHash:  "test-session-hash=",
			expectedUHash:  "test-user-hash==",
		},
		{
			name:           "只包含必需字段的Cookie",
			cookieStr:      `serviceToken=abc123; userId=12345; i.mi.com_slh=hash1; i.mi.com_ph=hash2`,
			wantErr:        false,
			expectedToken:  "abc123",
			expectedUserID: "12345",
			expectedSHash:  "hash1",
			expectedUHash:  "hash2",
		},
		{
			name:           "字段顺序不同",
			cookieStr:      `i.mi.com_ph=hash2; i.mi.com_slh=hash1; userId=12345; serviceToken=abc123`,
			wantErr:        false,
			expectedToken:  "abc123",
			expectedUserID: "12345",
			expectedSHash:  "hash1",
			expectedUHash:  "hash2",
		},
		{
			name:           "带额外空格",
			cookieStr:      `  serviceToken = abc123 ;  userId = 12345 ; i.mi.com_slh = hash1 ; i.mi.com_ph = hash2  `,
			wantErr:        false,
			expectedToken:  "abc123",
			expectedUserID: "12345",
			expectedSHash:  "hash1",
			expectedUHash:  "hash2",
		},
		{
			name:      "空字符串",
			cookieStr: "",
			wantErr:   true,
		},
		{
			name:      "缺少serviceToken",
			cookieStr: `userId=12345; i.mi.com_slh=hash1; i.mi.com_ph=hash2`,
			wantErr:   true,
		},
		{
			name:      "缺少userId",
			cookieStr: `serviceToken=abc123; i.mi.com_slh=hash1; i.mi.com_ph=hash2`,
			wantErr:   true,
		},
		{
			name:      "缺少i.mi.com_slh",
			cookieStr: `serviceToken=abc123; userId=12345; i.mi.com_ph=hash2`,
			wantErr:   true,
		},
		{
			name:      "缺少i.mi.com_ph",
			cookieStr: `serviceToken=abc123; userId=12345; i.mi.com_slh=hash1`,
			wantErr:   true,
		},
		{
			name:      "格式错误的Cookie",
			cookieStr: `invalidformat`,
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serviceToken, userId, sessionHash, userHash, err := ParseCookieString(tt.cookieStr)

			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCookieString() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if serviceToken != tt.expectedToken {
					t.Errorf("serviceToken = %v, want %v", serviceToken, tt.expectedToken)
				}
				if userId != tt.expectedUserID {
					t.Errorf("userId = %v, want %v", userId, tt.expectedUserID)
				}
				if sessionHash != tt.expectedSHash {
					t.Errorf("sessionHash = %v, want %v", sessionHash, tt.expectedSHash)
				}
				if userHash != tt.expectedUHash {
					t.Errorf("userHash = %v, want %v", userHash, tt.expectedUHash)
				}
			}
		})
	}
}
