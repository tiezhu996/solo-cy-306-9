// 活动关键信息变更提醒的接口层（HTTP）集成测试。
//
// 通过 router.New(...).Setup() 装配完整路由栈（JWT 认证、RBAC、审计、handler、service、
// repository），使用纯 Go 的内存 SQLite，httptest 直接发起请求，无需启动 MySQL/网络端口，
// `go test ./...` 可重复执行。SQL 为 GORM 通用写法，生产 MySQL 行为一致。
package router_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gbevent/internal/config"
	"gbevent/internal/constants"
	"gbevent/internal/handler"
	"gbevent/internal/model"
	"gbevent/internal/repository"
	"gbevent/internal/router"
	"gbevent/internal/service"
	"gbevent/internal/util"
)

const apiTestJWTSecret = "api-test-jwt-secret-please-change"

var apiTestDBCounter atomic.Uint64

// 测试用户（与 service 层测试口径一致）。
const (
	apiOrg   uint64 = 1 // 活动所属组织者
	apiOther uint64 = 2 // 无关组织者
	apiAdmin uint64 = 3 // 管理员
	apiReg   uint64 = 4 // 已报名（approved）
	apiPen   uint64 = 5 // 待审核（pending）
	apiChk   uint64 = 6 // 已签到（checked_in）
	apiCan   uint64 = 7 // 已取消（cancelled）
	apiRej   uint64 = 8 // 被拒绝（rejected）
	apiUser  uint64 = 9 // 普通用户
)

// apiEnv 封装一次测试所需的引擎、数据库与请求辅助。
type apiEnv struct {
	t      *testing.T
	db     *gorm.DB
	engine *gin.Engine
}

func newAPIEnv(t *testing.T) *apiEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dsn := fmt.Sprintf("file:api_%d?mode=memory&cache=shared", apiTestDBCounter.Add(1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.User{}, &model.Activity{}, &model.Registration{}, &model.CheckInRecord{},
		&model.Comment{}, &model.Favorite{}, &model.Notification{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := &config.Config{
		AppEnv: "test", ServerPort: "8080", JWTSecret: apiTestJWTSecret, JWTExpireHours: 2,
		RateLimitPerMinute: 100000, UploadDir: t.TempDir(), UploadMaxMB: 10,
		CORSOrigins: []string{"*"},
	}

	activityRepo := repository.NewActivityRepository(db)
	regRepo := repository.NewRegistrationRepository(db)
	checkinRepo := repository.NewCheckInRecordRepository(db)
	commentRepo := repository.NewCommentRepository(db)
	favoriteRepo := repository.NewFavoriteRepository(db)
	notifyRepo := repository.NewNotificationRepository(db)

	activitySvc := service.NewActivityService(db, activityRepo, regRepo, notifyRepo, checkinRepo, logger)
	regSvc := service.NewRegistrationService(db, regRepo, activitySvc, notifyRepo, logger)
	checkinSvc := service.NewCheckInRecordService(db, checkinRepo, regRepo, activitySvc, notifyRepo, logger)
	commentSvc := service.NewCommentService(commentRepo, activitySvc, logger)
	favoriteSvc := service.NewFavoriteService(favoriteRepo, activitySvc, logger)
	notifySvc := service.NewNotificationService(notifyRepo, logger)
	userSvc := service.NewUserService(repository.NewUserRepository(db), logger)

	r := router.New(cfg, db, logger,
		handler.NewUserHandler(userSvc, logger),
		handler.NewActivityHandler(activitySvc, logger),
		handler.NewRegistrationHandler(regSvc, logger),
		handler.NewCheckInRecordHandler(checkinSvc, logger),
		handler.NewCommentHandler(commentSvc, logger),
		handler.NewFavoriteHandler(favoriteSvc, logger),
		handler.NewNotificationHandler(notifySvc, logger),
		handler.NewUploadHandler(cfg, logger),
	)
	return &apiEnv{t: t, db: db, engine: r.Setup()}
}

// token 为某用户签发真实 JWT（与登录接口同一套签发逻辑）。
func token(userID uint64, role string) string {
	tok, err := util.GenerateToken(apiTestJWTSecret, time.Hour, userID, "u"+fmt.Sprint(userID), role)
	if err != nil {
		panic(err)
	}
	return tok
}

// req 发起 HTTP 请求；userID=0 表示不带 Authorization 头。
func (e *apiEnv) req(method, path string, userID uint64, role string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	r := httptest.NewRequest(method, path, reader)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if userID != 0 {
		r.Header.Set("Authorization", "Bearer "+token(userID, role))
	}
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, r)
	return w
}

// apiResp 统一响应体。
type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *apiEnv) decode(w *httptest.ResponseRecorder) apiResp {
	e.t.Helper()
	var out apiResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return out
}

// seedUsers 写入全部测试用户。
func (e *apiEnv) seedUsers() {
	users := []model.User{
		{ID: apiOrg, Username: "org", PasswordHash: "x", Role: constants.RoleOrganizer},
		{ID: apiOther, Username: "other-org", PasswordHash: "x", Role: constants.RoleOrganizer},
		{ID: apiAdmin, Username: "admin", PasswordHash: "x", Role: constants.RoleAdmin},
		{ID: apiReg, Username: "reg", PasswordHash: "x", Role: constants.RoleUser},
		{ID: apiPen, Username: "pen", PasswordHash: "x", Role: constants.RoleUser},
		{ID: apiChk, Username: "chk", PasswordHash: "x", Role: constants.RoleUser},
		{ID: apiCan, Username: "can", PasswordHash: "x", Role: constants.RoleUser},
		{ID: apiRej, Username: "rej", PasswordHash: "x", Role: constants.RoleUser},
		{ID: apiUser, Username: "plain", PasswordHash: "x", Role: constants.RoleUser},
	}
	if err := e.db.Create(&users).Error; err != nil {
		e.t.Fatalf("seed users: %v", err)
	}
}

// seedActivity 直接写入一条已发布活动，返回其引用。
func (e *apiEnv) seedActivity(id uint64, status string) *model.Activity {
	start := time.Now().Add(72 * time.Hour)
	a := &model.Activity{
		ID: id, Title: "接口测试原标题", ActivityType: constants.ActivityTypeLecture,
		StartTime: start, EndTime: start.Add(2 * time.Hour), Location: "原地点",
		Capacity: 100, SignupDeadline: start.Add(-time.Hour),
		Status: status, OrganizerID: apiOrg,
	}
	if err := e.db.Create(a).Error; err != nil {
		e.t.Fatalf("seed activity: %v", err)
	}
	return a
}

func (e *apiEnv) seedRegistration(activityID, userID uint64, status, review string) {
	r := &model.Registration{
		ActivityID: activityID, UserID: userID, Name: fmt.Sprintf("n%d", userID), Phone: "13900000000",
		VoucherNo: fmt.Sprintf("API-V-%d-%d", activityID, userID), Status: status, ReviewStatus: review,
	}
	if err := e.db.Create(r).Error; err != nil {
		e.t.Fatalf("seed registration: %v", err)
	}
}

// seedAllRegistrations 写入三类有效报名 + 取消者 + 被拒绝者。
func (e *apiEnv) seedAllRegistrations(activityID uint64) {
	e.seedRegistration(activityID, apiReg, constants.RegistrationStatusRegistered, constants.ReviewStatusApproved)
	e.seedRegistration(activityID, apiPen, constants.RegistrationStatusRegistered, constants.ReviewStatusPending)
	e.seedRegistration(activityID, apiChk, constants.RegistrationStatusCheckedIn, constants.ReviewStatusApproved)
	e.seedRegistration(activityID, apiCan, constants.RegistrationStatusCancelled, constants.ReviewStatusApproved)
	e.seedRegistration(activityID, apiRej, constants.RegistrationStatusRegistered, constants.ReviewStatusRejected)
}

// notifItem 通知列表元素。
type notifItem struct {
	ID               uint64 `json:"id"`
	NotificationType string `json:"notification_type"`
	Title            string `json:"title"`
	Content          string `json:"content"`
	IsRead           bool   `json:"is_read"`
	TypeText         string `json:"type_text"`
}

// listChangeNotifications 通过 HTTP 回读某用户的活动变更提醒。
func (e *apiEnv) listChangeNotifications(userID uint64) []notifItem {
	w := e.req(http.MethodGet, "/api/v1/notifications/mine?page=1&page_size=100", userID, constants.RoleUser, nil)
	if w.Code != http.StatusOK {
		e.t.Fatalf("list notifications status=%d body=%s", w.Code, w.Body.String())
	}
	var page struct {
		List []notifItem `json:"list"`
	}
	resp := e.decode(w)
	if err := json.Unmarshal(resp.Data, &page); err != nil {
		e.t.Fatalf("decode notification page: %v", err)
	}
	out := make([]notifItem, 0, len(page.List))
	for _, n := range page.List {
		if n.NotificationType == constants.NotificationActivityChange {
			out = append(out, n)
		}
	}
	return out
}

// notificationTotal 通过 HTTP 回读某用户通知总数。
func (e *apiEnv) notificationTotal(userID uint64) int64 {
	w := e.req(http.MethodGet, "/api/v1/notifications/mine?page=1&page_size=100", userID, constants.RoleUser, nil)
	if w.Code != http.StatusOK {
		e.t.Fatalf("list notifications status=%d", w.Code)
	}
	var page struct {
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(e.decode(w).Data, &page); err != nil {
		e.t.Fatalf("decode page: %v", err)
	}
	return page.Total
}

// getActivity 通过 DB 核对活动数据是否被改动。
func (e *apiEnv) getActivity(id uint64) *model.Activity {
	var a model.Activity
	if err := e.db.First(&a, id).Error; err != nil {
		e.t.Fatalf("find activity: %v", err)
	}
	return &a
}

// 1. 未登录编辑：401，活动不变，不发送任何提醒。
func TestAPI_UpdateActivity_UnauthenticatedRejected(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()
	a := e.seedActivity(100, constants.ActivityStatusPublished)
	e.seedAllRegistrations(100)

	w := e.req(http.MethodPut, "/api/v1/activities/100", 0, "", map[string]any{"title": "黑客标题"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", w.Code, w.Body.String())
	}
	if e.decode(w).Code != constants.CodeUnauthorized {
		t.Fatalf("code = %d, want %d", e.decode(w).Code, constants.CodeUnauthorized)
	}
	got := e.getActivity(100)
	if got.Title != a.Title || got.Location != a.Location || got.Capacity != a.Capacity {
		t.Fatalf("activity must be unchanged after unauthenticated edit: %+v", got)
	}
	for _, uid := range []uint64{apiReg, apiPen, apiChk, apiCan, apiRej} {
		if n := e.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("no notification may be sent after 401, user %d got %d", uid, len(n))
		}
	}
}

// 1b. 伪造/过期 token 编辑：401 且不发送。
func TestAPI_UpdateActivity_InvalidTokenRejected(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()
	e.seedActivity(108, constants.ActivityStatusPublished)
	e.seedAllRegistrations(108)

	r := httptest.NewRequest(http.MethodPut, "/api/v1/activities/108",
		bytes.NewReader([]byte(`{"title":"伪造"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer not-a-real-jwt")
	w := httptest.NewRecorder()
	e.engine.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", w.Code, w.Body.String())
	}
	for _, uid := range []uint64{apiReg, apiPen, apiChk} {
		if n := e.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("invalid token edit must not notify uid=%d, got %d", uid, len(n))
		}
	}
}

// 1c. 名额传负数：400（omitempty 只跳过未传，不放松 min=0 下限）。
func TestAPI_UpdateActivity_NegativeCapacityRejected(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()
	a := e.seedActivity(109, constants.ActivityStatusPublished)
	e.seedAllRegistrations(109)

	w := e.req(http.MethodPut, "/api/v1/activities/109", apiOrg, constants.RoleOrganizer,
		map[string]any{"capacity": -1})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", w.Code, w.Body.String())
	}
	if e.getActivity(109).Capacity != a.Capacity {
		t.Fatal("capacity must be unchanged after validation failure")
	}
	for _, uid := range []uint64{apiReg, apiPen, apiChk} {
		if n := e.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("failed validation must not notify uid=%d, got %d", uid, len(n))
		}
	}
}

// 2. 无关组织者 / 普通用户编辑：403，活动不变，不发送提醒。
func TestAPI_UpdateActivity_NonOwnerForbidden(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()
	a := e.seedActivity(101, constants.ActivityStatusPublished)
	e.seedAllRegistrations(101)

	cases := []struct {
		name string
		uid  uint64
		role string
	}{
		{"unrelated organizer", apiOther, constants.RoleOrganizer},
		{"plain user", apiUser, constants.RoleUser},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := e.req(http.MethodPut, "/api/v1/activities/101", tc.uid, tc.role, map[string]any{
				"title": "越权标题", "location": "越权地点", "capacity": 1,
			})
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403, body=%s", w.Code, w.Body.String())
			}
			if e.decode(w).Code != constants.CodeForbidden {
				t.Fatalf("code = %d, want %d", e.decode(w).Code, constants.CodeForbidden)
			}
			got := e.getActivity(101)
			if got.Title != a.Title || got.Location != a.Location || got.Capacity != a.Capacity {
				t.Fatalf("activity must be unchanged after forbidden edit: %+v", got)
			}
			for _, uid := range []uint64{apiReg, apiPen, apiChk, apiCan, apiRej, tc.uid} {
				if n := e.listChangeNotifications(uid); len(n) != 0 {
					t.Fatalf("no notification may be sent after 403, user %d got %d", uid, len(n))
				}
			}
		})
	}
}

// 3. 组织者正常编辑五字段：三类有效报名者各收一条合并提醒，取消/被拒绝者不收。
func TestAPI_UpdateActivity_OwnerNotifiesValidOnly(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()
	a := e.seedActivity(102, constants.ActivityStatusPublished)
	e.seedAllRegistrations(102)

	newStart := a.StartTime.Add(24 * time.Hour)
	newEnd := newStart.Add(2 * time.Hour)
	w := e.req(http.MethodPut, "/api/v1/activities/102", apiOrg, constants.RoleOrganizer, map[string]any{
		"title": "接口测试新标题", "start_time": newStart.Format(time.RFC3339),
		"end_time": newEnd.Format(time.RFC3339), "location": "新地点", "capacity": 180,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	got := e.getActivity(102)
	if got.Title != "接口测试新标题" || got.Location != "新地点" || got.Capacity != 180 {
		t.Fatalf("fields not persisted: %+v", got)
	}

	for idx, uid := range []uint64{apiReg, apiPen, apiChk} {
		notes := e.listChangeNotifications(uid)
		if len(notes) != 1 {
			t.Fatalf("valid recipient #%d (uid=%d) expected 1 notification, got %d", idx, uid, len(notes))
		}
		n := notes[0]
		if n.IsRead {
			t.Errorf("uid=%d notification must be unread", uid)
		}
		if n.TypeText != "活动变更" || n.Title != constants.MsgActivityChanged {
			t.Errorf("uid=%d wrong type/title: %+v", uid, n)
		}
		for _, want := range []string{"接口测试原标题", "接口测试新标题", "原地点", "新地点", "100", "180", "标题", "开始时间", "结束时间", "地点", "名额"} {
			if !containsStr(n.Content, want) {
				t.Errorf("uid=%d content missing %q: %s", uid, want, n.Content)
			}
		}
	}
	for _, uid := range []uint64{apiCan, apiRej} {
		if n := e.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("excluded uid=%d must not be notified, got %d", uid, len(n))
		}
	}
}

// 4. 管理员编辑他人活动成功，且只通知有效报名者。
func TestAPI_UpdateActivity_AdminNotifiesValidOnly(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()
	e.seedActivity(103, constants.ActivityStatusPublished)
	e.seedAllRegistrations(103)

	w := e.req(http.MethodPut, "/api/v1/activities/103", apiAdmin, constants.RoleAdmin, map[string]any{
		"title": "管理员改标题",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if e.getActivity(103).Title != "管理员改标题" {
		t.Fatal("title not persisted by admin")
	}
	for _, uid := range []uint64{apiReg, apiPen, apiChk} {
		if n := e.listChangeNotifications(uid); len(n) != 1 {
			t.Fatalf("admin edit: valid uid=%d expected 1 notification, got %d", uid, len(n))
		}
	}
	for _, uid := range []uint64{apiCan, apiRej} {
		if n := e.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("admin edit: excluded uid=%d got %d notifications", uid, len(n))
		}
	}
}

// 5. 草稿活动经接口编辑不发送提醒。
func TestAPI_UpdateActivity_DraftSendsNothing(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()
	e.seedActivity(104, constants.ActivityStatusDraft)
	e.seedAllRegistrations(104)

	w := e.req(http.MethodPut, "/api/v1/activities/104", apiOrg, constants.RoleOrganizer, map[string]any{
		"title": "草稿改标题", "location": "草稿地点",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	for _, uid := range []uint64{apiReg, apiPen, apiChk, apiCan, apiRej} {
		if n := e.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("draft edit must not notify uid=%d, got %d", uid, len(n))
		}
	}
}

// 6. 通知回读 + 单条已读：只能标记自己的，别人的不受影响；未登录 401。
func TestAPI_Notifications_ReadbackAndMarkRead(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()
	e.seedActivity(105, constants.ActivityStatusPublished)
	e.seedAllRegistrations(105)

	if w := e.req(http.MethodPut, "/api/v1/activities/105", apiOrg, constants.RoleOrganizer,
		map[string]any{"location": "回读测试地点"}); w.Code != http.StatusOK {
		t.Fatalf("update: %s", w.Body.String())
	}

	regNotes := e.listChangeNotifications(apiReg)
	if len(regNotes) != 1 {
		t.Fatalf("setup: expected 1 notification, got %d", len(regNotes))
	}
	nid := regNotes[0].ID

	// 未登录回读被拒。
	if w := e.req(http.MethodGet, "/api/v1/notifications/mine", 0, "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("mine without token status=%d", w.Code)
	}
	// 未登录标记已读被拒。
	if w := e.req(http.MethodPost, fmt.Sprintf("/api/v1/notifications/%d/read", nid), 0, "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("mark read without token status=%d", w.Code)
	}

	// apiPen 尝试标记 apiReg 的通知：接口按 user_id 过滤，返回成功但不改动 apiReg 的数据。
	if w := e.req(http.MethodPost, fmt.Sprintf("/api/v1/notifications/%d/read", nid), apiPen, constants.RoleUser, nil); w.Code != http.StatusOK {
		t.Fatalf("cross-user mark read status=%d body=%s", w.Code, w.Body.String())
	}
	if after := e.listChangeNotifications(apiReg); after[0].IsRead {
		t.Fatal("one user must not be able to mark another user's notification read")
	}

	// 本人标记自己的已读，回读确认。
	if w := e.req(http.MethodPost, fmt.Sprintf("/api/v1/notifications/%d/read", nid), apiReg, constants.RoleUser, nil); w.Code != http.StatusOK {
		t.Fatalf("self mark read status=%d", w.Code)
	}
	if after := e.listChangeNotifications(apiReg); !after[0].IsRead {
		t.Fatal("notification should be read after self mark-read")
	}
}

// 7. 全部已读只影响本人，且通知本身仍可回读。
func TestAPI_Notifications_MarkAllReadScoped(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()
	e.seedActivity(106, constants.ActivityStatusPublished)
	e.seedAllRegistrations(106)

	// 两次编辑，每位有效者有两条未读变更提醒。
	for _, loc := range []string{"地点一", "地点二"} {
		if w := e.req(http.MethodPut, "/api/v1/activities/106", apiOrg, constants.RoleOrganizer,
			map[string]any{"location": loc}); w.Code != http.StatusOK {
			t.Fatalf("update: %s", w.Body.String())
		}
	}
	if w := e.req(http.MethodPost, "/api/v1/notifications/read-all", apiReg, constants.RoleUser, nil); w.Code != http.StatusOK {
		t.Fatalf("read-all status=%d", w.Code)
	}
	for _, n := range e.listChangeNotifications(apiReg) {
		if !n.IsRead {
			t.Fatal("all notifications should be read after read-all")
		}
	}
	// 不影响其他用户。
	for _, n := range e.listChangeNotifications(apiPen) {
		if n.IsRead {
			t.Fatal("read-all must be scoped to the requesting user")
		}
	}
}

// 8. 连续多轮编辑 + 回读稳定：每轮三类有效者各加一条，取消/被拒绝者始终为 0。
func TestAPI_ConsecutiveEdits_StableAcrossRounds(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()
	e.seedActivity(107, constants.ActivityStatusPublished)
	e.seedAllRegistrations(107)

	rounds := []map[string]any{
		{"title": "轮次二标题"},
		{"location": "轮次二地点"},
		{"capacity": 12},
	}
	for i, body := range rounds {
		if w := e.req(http.MethodPut, "/api/v1/activities/107", apiOrg, constants.RoleOrganizer, body); w.Code != http.StatusOK {
			t.Fatalf("round %d status=%d body=%s", i+1, w.Code, w.Body.String())
		}
		for _, uid := range []uint64{apiReg, apiPen, apiChk} {
			if n := e.listChangeNotifications(uid); len(n) != i+1 {
				t.Fatalf("round %d uid=%d expected %d notifications, got %d", i+1, uid, i+1, len(n))
			}
		}
		for _, uid := range []uint64{apiCan, apiRej} {
			if n := e.listChangeNotifications(uid); len(n) != 0 {
				t.Fatalf("round %d excluded uid=%d got %d", i+1, uid, len(n))
			}
		}
	}
}

// 9. 回归：发布、在线报名、审核、凭证签到、导出接口端到端可用，
// 且这些流程本身不产生活动变更提醒。
func TestAPI_Regression_PublishSignupReviewCheckinExport(t *testing.T) {
	e := newAPIEnv(t)
	e.seedUsers()

	// 组织者通过接口创建草稿活动。
	start := time.Now().Add(72 * time.Hour)
	createBody := map[string]any{
		"title": "接口回归活动", "activity_type": "lecture",
		"start_time": start.Format(time.RFC3339), "end_time": start.Add(2 * time.Hour).Format(time.RFC3339),
		"signup_deadline": start.Add(-time.Hour).Format(time.RFC3339),
		"location":        "回归会场", "capacity": 100, "status": "draft",
	}
	w := e.req(http.MethodPost, "/api/v1/activities", apiOrg, constants.RoleOrganizer, createBody)
	if w.Code != http.StatusOK {
		t.Fatalf("create activity: %s", w.Body.String())
	}
	var created model.Activity
	if err := json.Unmarshal(e.decode(w).Data, &created); err != nil {
		t.Fatalf("decode created activity: %v", err)
	}
	actID := created.ID

	// 未发布前报名应失败。
	w = e.req(http.MethodPost, "/api/v1/registrations", apiReg, constants.RoleUser, map[string]any{
		"activity_id": actID, "name": "Alice", "phone": "13900000001",
	})
	if w.Code == http.StatusOK {
		t.Fatal("signup on draft activity must fail")
	}

	// 发布。
	if w := e.req(http.MethodPost, fmt.Sprintf("/api/v1/activities/%d/publish", actID), apiOrg, constants.RoleOrganizer, nil); w.Code != http.StatusOK {
		t.Fatalf("publish status=%d body=%s", w.Code, w.Body.String())
	}

	// Alice、Bob 在线报名。
	signup := func(uid uint64, name, phone string) model.Registration {
		w := e.req(http.MethodPost, "/api/v1/registrations", uid, constants.RoleUser, map[string]any{
			"activity_id": actID, "name": name, "phone": phone,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("signup %s: %s", name, w.Body.String())
		}
		var reg model.Registration
		if err := json.Unmarshal(e.decode(w).Data, &reg); err != nil {
			t.Fatalf("decode signup: %v", err)
		}
		return reg
	}
	alice := signup(apiReg, "Alice", "13900000001")
	bob := signup(apiPen, "Bob", "13900000002")
	if alice.VoucherNo == "" || bob.VoucherNo == "" {
		t.Fatal("voucher numbers must be generated")
	}
	// 重复报名被拒。
	if w := e.req(http.MethodPost, "/api/v1/registrations", apiReg, constants.RoleUser, map[string]any{
		"activity_id": actID, "name": "Alice", "phone": "13900000001",
	}); w.Code == http.StatusOK {
		t.Fatal("duplicate signup must fail")
	}

	// 组织者审核通过 Bob。
	if w := e.req(http.MethodPost, fmt.Sprintf("/api/v1/registrations/%d/review", bob.ID), apiOrg, constants.RoleOrganizer,
		map[string]any{"review_status": "approved"}); w.Code != http.StatusOK {
		t.Fatalf("review status=%d body=%s", w.Code, w.Body.String())
	}
	// 普通用户无权审核（RBAC 403）。
	if w := e.req(http.MethodPost, fmt.Sprintf("/api/v1/registrations/%d/review", bob.ID), apiUser, constants.RoleUser,
		map[string]any{"review_status": "rejected"}); w.Code != http.StatusForbidden {
		t.Fatalf("review by plain user status=%d, want 403", w.Code)
	}

	// 组织者凭证号签到 Alice。
	w = e.req(http.MethodPost, fmt.Sprintf("/api/v1/check-ins?activity_id=%d", actID), apiOrg, constants.RoleOrganizer,
		map[string]any{"voucher": alice.VoucherNo})
	if w.Code != http.StatusOK {
		t.Fatalf("checkin status=%d body=%s", w.Code, w.Body.String())
	}
	// 重复签到被拒。
	w = e.req(http.MethodPost, fmt.Sprintf("/api/v1/check-ins?activity_id=%d", actID), apiOrg, constants.RoleOrganizer,
		map[string]any{"voucher": alice.VoucherNo})
	if w.Code == http.StatusOK {
		t.Fatal("duplicate checkin must fail")
	}

	// 通知回归：Alice 报名成功 + 签到成功 = 2；Bob 报名成功 + 审核结果 = 2；全程零变更提醒。
	if total := e.notificationTotal(apiReg); total != 2 {
		t.Errorf("alice notifications=%d, want 2", total)
	}
	if total := e.notificationTotal(apiPen); total != 2 {
		t.Errorf("bob notifications=%d, want 2", total)
	}
	for _, uid := range []uint64{apiReg, apiPen} {
		if n := e.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("publish/signup/review/checkin must not create change notifications, uid=%d got %d", uid, len(n))
		}
	}

	// 导出 CSV：组织者成功且包含两人凭证号；普通用户 403；未登录 401。
	w = e.req(http.MethodGet, fmt.Sprintf("/api/v1/registrations/export?activity_id=%d", actID), apiOrg, constants.RoleOrganizer, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !containsStr(ct, "text/csv") {
		t.Errorf("export content-type=%q", ct)
	}
	body := w.Body.String()
	if !containsStr(body, alice.VoucherNo) || !containsStr(body, bob.VoucherNo) {
		t.Errorf("csv must contain both vouchers:\n%s", body)
	}
	if w := e.req(http.MethodGet, fmt.Sprintf("/api/v1/registrations/export?activity_id=%d", actID), apiUser, constants.RoleUser, nil); w.Code != http.StatusForbidden {
		t.Fatalf("export by plain user status=%d, want 403", w.Code)
	}
	if w := e.req(http.MethodGet, fmt.Sprintf("/api/v1/registrations/export?activity_id=%d", actID), 0, "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("export without token status=%d, want 401", w.Code)
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexStr(s, sub) >= 0)
}

func indexStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
