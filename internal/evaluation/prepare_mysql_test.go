//go:build evaluationlive

package evaluation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"video-support-agent/internal/config"
	"video-support-agent/internal/confirmation"
	"video-support-agent/internal/model"
	"video-support-agent/internal/repository"
	"video-support-agent/internal/service"
)

func TestPrepareChildDeadlineWithMySQLLock(t *testing.T) {
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("TEST_ISOLATED") != "1" || os.Getenv("DB_NAME") != "eval_retrieval_test" || os.Getenv("EVALUATION_EVIDENCE") == "" {
		t.Fatal("isolated script required")
	}
	d := driver.NewConfig()
	d.Net = "tcp"
	d.Addr = net.JoinHostPort("127.0.0.1", os.Getenv("DB_PORT"))
	d.User = os.Getenv("DB_USER")
	d.Passwd = os.Getenv("DB_PASSWORD")
	d.DBName = os.Getenv("DB_NAME")
	d.ParseTime = true
	d.Timeout = 5 * time.Second
	db, err := gorm.Open(mysql.Open(d.FormatDSN()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(err)
	pool, err := db.DB()
	must(err)
	defer pool.Close()
	var identity struct{ Name, UUID string }
	must(db.Raw("SELECT DATABASE() AS name, @@server_uuid AS uuid").Scan(&identity).Error)
	if identity.Name != d.DBName || identity.UUID != os.Getenv("TEST_MYSQL_SERVER_UUID") {
		t.Fatal("database mismatch")
	}
	user := model.User{Username: "prepare-deadline-test", PasswordHash: "disabled", Role: model.RoleUser}
	must(db.Create(&user).Error)
	conversation := model.Conversation{UserID: user.ID, Title: "prepare deadline test"}
	must(db.Omit("User").Create(&conversation).Error)
	store, err := repository.NewAppealTicketRepository(db)
	must(err)
	eligibility, err := service.NewAppealEligibilityService(service.NoAppealPolicies{})
	must(err)
	signer, err := confirmation.NewAppealSigner(config.AppealConfirmationConfig{KeyBase64: base64.StdEncoding.EncodeToString(make([]byte, 32)), Issuer: "deadline-test", Audience: "deadline-test"})
	must(err)
	appeals, err := service.NewAppealTicketService(store, eligibility, signer)
	must(err)
	runner, c := projectionFixture()
	runner.appeals = appeals
	parent, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	lock, err := pool.Conn(parent)
	must(err)
	defer lock.Close()
	_, err = lock.ExecContext(parent, "LOCK TABLES conversations WRITE")
	must(err)
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_, err := lock.ExecContext(cleanup, "UNLOCK TABLES")
		if err != nil {
			t.Error(err)
		}
	}()
	started := time.Now()
	attempt, ticket := runner.confirmFixture(parent, "prepare-deadline", c, service.AgentAuditOwner{UserID: user.ID, ConversationID: conversation.ID}, service.CreateAppealInput{SubjectType: model.AppealSubjectContent, SubjectRef: "synthetic", Reason: "合成测试", IntakeMode: model.AppealIntakeManualReview})
	elapsed := time.Since(started)
	if attempt.Observation.Status != RunTimedOut || attempt.ErrorCode != "timed_out" || ticket != 0 || parent.Err() != nil || elapsed < 14*time.Second || elapsed > 25*time.Second {
		t.Fatalf("child deadline classification status=%s code=%s elapsed=%s parent=%v", attempt.Observation.Status, attempt.ErrorCode, elapsed, parent.Err())
	}
	result := map[string]any{"status": attempt.Observation.Status, "error_code": attempt.ErrorCode, "elapsed_ms": elapsed.Milliseconds(), "parent_alive": parent.Err() == nil, "ticket_id": ticket, "fault": "real MySQL table lock held by independent connection"}
	b, err := json.MarshalIndent(result, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(os.Getenv("EVALUATION_EVIDENCE"), "prepare-deadline.json"), b, 0600))
}
