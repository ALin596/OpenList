package xiaomi_cloud

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/db"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestAccountCredentialFields(t *testing.T) {
	items := op.GetDriverInfoMap()[config.Name].Additional
	fields := make(map[string]bool)
	for _, item := range items {
		fields[item.Name] = item.Required
	}
	if !fields["pass_token"] || !fields["user_id"] {
		t.Fatal("配置页面缺少两个必填的独立账号输入框")
	}
}

func TestAccountCredentialsValidation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		addition  Addition
		wantError string
	}{
		{"缺少令牌", Addition{UserID: "123456"}, "passToken 不能为空"},
		{"缺少账号", Addition{PassToken: "token"}, "userId 不能为空"},
		{"误填完整 Cookie", Addition{PassToken: "token; userId=123456", UserID: "123456"}, "passToken 格式无效"},
		{"令牌包含换行", Addition{PassToken: "token\n", UserID: "123456"}, "passToken 格式无效"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := &XiaomiCloud{Addition: tc.addition}
			if err := driver.Init(context.Background()); err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("初始化错误 = %v，期望包含 %s", err, tc.wantError)
			}
		})
	}
}

func TestRotatedCredentialsPersistAcrossRestart(t *testing.T) {
	previousConfig := conf.Conf
	conf.Conf = conf.DefaultConfig(t.TempDir())
	t.Cleanup(func() { conf.Conf = previousConfig })
	database, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "credentials.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	db.Init(database)
	t.Cleanup(db.Close)

	var loginCount atomic.Int32
	installAuthFlow(t, &loginCount, "account-pass")
	storage := model.Storage{Driver: config.Name, MountPath: "/xiaomi"}
	if err := db.CreateStorage(&storage); err != nil {
		t.Fatal(err)
	}
	driver := &XiaomiCloud{Storage: storage, Addition: Addition{PassToken: "account-pass", UserID: "123456"}}
	if err := driver.ensureServiceToken(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetStorageById(storage.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 新实例从数据库读取独立凭据，重启后直接用轮换后的令牌获取云盘会话。
	restarted := &XiaomiCloud{Storage: *stored}
	if err := utils.Json.UnmarshalFromString(stored.Addition, restarted.GetAddition()); err != nil {
		t.Fatal(err)
	}
	if restarted.PassToken != "renewed-pass" || restarted.UserID != "123456" {
		t.Fatal("数据库未保存轮换后的账号凭据")
	}
	var restartedLoginCount atomic.Int32
	installAuthFlow(t, &restartedLoginCount, "renewed-pass")
	if err := restarted.ensureServiceToken(context.Background(), ""); err != nil {
		t.Fatalf("重启后使用持久化凭据登录失败: %v", err)
	}
	if !restarted.sessionSnapshot().complete() || restartedLoginCount.Load() != 1 {
		t.Fatal("重启后未重新获取完整的云盘会话")
	}
}
