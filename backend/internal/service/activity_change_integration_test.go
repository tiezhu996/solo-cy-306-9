// 活动关键信息变更提醒的端到端集成测试。
//
// 使用纯 Go 的 glebarez/sqlite（modernc，无需 CGO/MySQL）装配真实的
// repository + service，因此 `go test ./...` 在任意环境都可重复执行；
// 测试中的 SQL 均为 GORM 通用写法，FOR UPDATE 在 MySQL 上语义不变。
package service_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gbevent/internal/constants"
	"gbevent/internal/model"
	"gbevent/internal/repository"
	"gbevent/internal/service"
	"gbevent/internal/util"
)

var changeTestDBCounter atomic.Uint64

// changeFixture 装配一套真实的 service 层（每个用例独立内存数据库）。
type changeFixture struct {
	t           *testing.T
	db          *gorm.DB
	activitySvc *service.ActivityService
	regSvc      *service.RegistrationService
	notifySvc   *service.NotificationService
	checkinSvc  *service.CheckInRecordService
	commentSvc  *service.CommentService
	favoriteSvc *service.FavoriteService
}

func newChangeFixture(t *testing.T) *changeFixture {
	t.Helper()
	dsn := fmt.Sprintf("file:change_%d?mode=memory&cache=shared", changeTestDBCounter.Add(1))
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

	return &changeFixture{
		t: t, db: db, activitySvc: activitySvc, regSvc: regSvc, notifySvc: notifySvc,
		checkinSvc: checkinSvc, commentSvc: commentSvc, favoriteSvc: favoriteSvc,
	}
}

const (
	orgID  uint64 = 1 // 组织者
	userAP uint64 = 2 // 已报名（已通过审核）
	userBP uint64 = 3 // 待审核（registered + pending）
	userCC uint64 = 4 // 已签到（checked_in + approved）
	userXX uint64 = 5 // 已取消（cancelled）
	userRJ uint64 = 6 // 已拒绝（registered + rejected，审核拒绝不改变 status）
)

// 不应收到活动变更提醒的用户（取消者、被拒绝者）。
var excludedFromChange = []uint64{userXX, userRJ}

// seedUsers 写入组织者与五类报名用户。
func (f *changeFixture) seedUsers() {
	users := []model.User{
		{ID: orgID, Username: "org", PasswordHash: "x", Nickname: "组织者", Role: constants.RoleOrganizer},
		{ID: userAP, Username: "alice", PasswordHash: "x", Nickname: "Alice", Role: constants.RoleUser},
		{ID: userBP, Username: "bob", PasswordHash: "x", Nickname: "Bob", Role: constants.RoleUser},
		{ID: userCC, Username: "carol", PasswordHash: "x", Nickname: "Carol", Role: constants.RoleUser},
		{ID: userXX, Username: "dave", PasswordHash: "x", Nickname: "Dave", Role: constants.RoleUser},
		{ID: userRJ, Username: "erin", PasswordHash: "x", Nickname: "Erin", Role: constants.RoleUser},
	}
	if err := f.db.Create(&users).Error; err != nil {
		f.t.Fatalf("seed users: %v", err)
	}
}

// seedActivity 直接写入一条活动，默认已发布。
func (f *changeFixture) seedActivity(id uint64, status string) *model.Activity {
	start := time.Now().Add(72 * time.Hour)
	a := &model.Activity{
		ID: id, Title: "原标题", Description: "原描述", CoverImage: "",
		ActivityType:   constants.ActivityTypeLecture,
		StartTime:      start,
		EndTime:        start.Add(2 * time.Hour),
		Location:       "原地点 A 座",
		Capacity:       100,
		SignupDeadline: start.Add(-time.Hour),
		Status:         status,
		OrganizerID:    orgID,
	}
	if err := f.db.Create(a).Error; err != nil {
		f.t.Fatalf("seed activity: %v", err)
	}
	return a
}

// seedRegistration 直接写入指定状态的报名。
func (f *changeFixture) seedRegistration(activityID, userID uint64, status, review string) *model.Registration {
	r := &model.Registration{
		ActivityID:   activityID,
		UserID:       userID,
		Name:         fmt.Sprintf("user-%d", userID),
		Phone:        "13900000000",
		VoucherNo:    fmt.Sprintf("V-%d-%d", activityID, userID),
		Status:       status,
		ReviewStatus: review,
	}
	if err := f.db.Create(r).Error; err != nil {
		f.t.Fatalf("seed registration: %v", err)
	}
	return r
}

// seedAllKindsOfRegistrations 写入三类有效报名 + 一个取消者 + 一个被拒绝者。
func (f *changeFixture) seedAllKindsOfRegistrations(activityID uint64) {
	f.seedRegistration(activityID, userAP, constants.RegistrationStatusRegistered, constants.ReviewStatusApproved)
	f.seedRegistration(activityID, userBP, constants.RegistrationStatusRegistered, constants.ReviewStatusPending)
	f.seedRegistration(activityID, userCC, constants.RegistrationStatusCheckedIn, constants.ReviewStatusApproved)
	f.seedRegistration(activityID, userXX, constants.RegistrationStatusCancelled, constants.ReviewStatusApproved)
	f.seedRegistration(activityID, userRJ, constants.RegistrationStatusRegistered, constants.ReviewStatusRejected)
}

// validChangeRecipients 三类有效报名者，顺序固定：已报名 → 待审核 → 已签到。
var validChangeRecipients = []uint64{userAP, userBP, userCC}

// assertExcludedGetNoChange 断言取消者/被拒绝者都没有收到活动变更提醒。
func (f *changeFixture) assertExcludedGetNoChange() {
	for _, uid := range excludedFromChange {
		if n := f.listChangeNotifications(uid); len(n) != 0 {
			f.t.Fatalf("excluded user %d must not be notified, got %d notifications", uid, len(n))
		}
	}
}

// listChangeNotifications 回读某用户的「活动变更」提醒（按 created_at DESC）。
func (f *changeFixture) listChangeNotifications(userID uint64) []map[string]any {
	list, _, err := f.notifySvc.ListMine(userID, 1, 100)
	if err != nil {
		f.t.Fatalf("list notifications for %d: %v", userID, err)
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		m := item.(map[string]any)
		if m["notification_type"] == constants.NotificationActivityChange {
			out = append(out, m)
		}
	}
	return out
}

func (f *changeFixture) notificationCount(userID uint64) int64 {
	_, total, err := f.notifySvc.ListMine(userID, 1, 100)
	if err != nil {
		f.t.Fatalf("count notifications for %d: %v", userID, err)
	}
	return total
}

func (f *changeFixture) findActivity(id uint64) *model.Activity {
	var a model.Activity
	if err := f.db.First(&a, id).Error; err != nil {
		f.t.Fatalf("find activity %d: %v", id, err)
	}
	return &a
}

// 场景 1：五个关键字段同时修改，已报名/待审核/已签到各收到一条合并提醒，取消者不收。
func TestActivityChange_AllKeyFieldsMergedIntoOne(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	a := f.seedActivity(100, constants.ActivityStatusPublished)
	f.seedAllKindsOfRegistrations(100)

	newStart := a.StartTime.Add(24 * time.Hour)
	newEnd := a.EndTime.Add(48 * time.Hour)
	fields := map[string]any{
		"title":      "新标题",
		"start_time": newStart,
		"end_time":   newEnd,
		"location":   "新地点 B 座",
		"capacity":   200,
	}
	updated, err := f.activitySvc.Update(100, orgID, constants.RoleOrganizer, fields)
	if err != nil {
		t.Fatalf("update activity: %v", err)
	}

	// 字段确实落库。
	if updated.Title != "新标题" || updated.Location != "新地点 B 座" || updated.Capacity != 200 {
		t.Fatalf("fields not persisted: %+v", updated)
	}
	if !updated.StartTime.Equal(newStart) || !updated.EndTime.Equal(newEnd) {
		t.Fatalf("time fields not persisted: start=%v end=%v", updated.StartTime, updated.EndTime)
	}

	// 三位有效报名者各收到恰好一条，且为未读、多字段合并、含旧值与新值。
	for _, uid := range validChangeRecipients {
		notes := f.listChangeNotifications(uid)
		if len(notes) != 1 {
			t.Fatalf("user %d: expected 1 merged notification, got %d", uid, len(notes))
		}
		n := notes[0]
		if n["is_read"].(bool) {
			t.Errorf("user %d: new notification must be unread", uid)
		}
		if n["title"] != constants.MsgActivityChanged {
			t.Errorf("user %d: title = %v, want %q", uid, n["title"], constants.MsgActivityChanged)
		}
		if n["type_text"] != "活动变更" {
			t.Errorf("user %d: type_text = %v, want 活动变更", uid, n["type_text"])
		}
		content := n["content"].(string)
		for _, want := range []string{
			"原标题", "新标题",
			util.FormatDateTime(a.StartTime), util.FormatDateTime(newStart),
			util.FormatDateTime(a.EndTime), util.FormatDateTime(newEnd),
			"原地点 A 座", "新地点 B 座",
			"100", "200",
			"标题", "开始时间", "结束时间", "地点", "名额",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("user %d: merged content %q missing %q", uid, content, want)
			}
		}
		if c := strings.Count(content, "关键信息发生变更"); c != 1 {
			t.Errorf("user %d: five fields must be merged into ONE body, marker count=%d", uid, c)
		}
	}

	// 取消者与被拒绝者都不收，且没有任何通知。
	f.assertExcludedGetNoChange()
	if total := f.notificationCount(userXX); total != 0 {
		t.Fatalf("cancelled user must have zero notifications, got %d", total)
	}
	if total := f.notificationCount(userRJ); total != 0 {
		t.Fatalf("rejected user must have zero new notifications, got %d", total)
	}
}

// 场景 2：草稿活动修改关键字段不发提醒。
func TestActivityChange_DraftDoesNotNotify(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	f.seedActivity(101, constants.ActivityStatusDraft)
	f.seedAllKindsOfRegistrations(101)

	if _, err := f.activitySvc.Update(101, orgID, constants.RoleOrganizer, map[string]any{
		"title": "草稿新标题", "location": "草稿新地点", "capacity": 50,
	}); err != nil {
		t.Fatalf("update draft: %v", err)
	}
	for _, uid := range []uint64{userAP, userBP, userCC, userXX, userRJ} {
		if n := f.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("draft edit must not notify user %d, got %d", uid, len(n))
		}
	}
}

// 场景 3：已结束活动修改关键字段不发提醒（结束本身也不发）。
func TestActivityChange_EndedDoesNotNotify(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	a := f.seedActivity(102, constants.ActivityStatusPublished)
	f.seedAllKindsOfRegistrations(102)

	// published -> ended。
	if _, err := f.activitySvc.End(102, orgID, constants.RoleOrganizer); err != nil {
		t.Fatalf("end: %v", err)
	}
	if a.Status == constants.ActivityStatusEnded {
		t.Fatal("seed activity should not be mutated by End")
	}
	// 结束后再编辑关键字段。
	if _, err := f.activitySvc.Update(102, orgID, constants.RoleOrganizer, map[string]any{
		"title": "结束后改标题", "location": "结束后改地点",
	}); err != nil {
		t.Fatalf("update ended: %v", err)
	}
	for _, uid := range validChangeRecipients {
		if n := f.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("ended activity edit/end must not notify user %d, got %d", uid, len(n))
		}
	}
}

// 场景 3b（缺陷复现）：走真实审核拒绝流程后，组织者再编辑关键字段，
// 被拒绝者不再收到新的变更提醒，其既有报名/审核通知仍可回读；
// 已报名、待审核、已签到三类人员照常各收一条。
func TestActivityChange_RejectedRegistrationExcludedAfterReview(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	a := f.seedActivity(110, constants.ActivityStatusPublished)

	// Erin 走真实在线报名（registered + pending，收到报名成功通知）。
	erinReg, err := f.regSvc.Create(a.ID, userRJ, "Erin", "13900000005", "")
	if err != nil {
		t.Fatalf("erin signup: %v", err)
	}
	// 组织者审核拒绝（只改 review_status=rejected，status 仍为 registered）。
	if _, err := f.regSvc.Review(erinReg.ID, orgID, constants.RoleOrganizer, constants.ReviewStatusRejected); err != nil {
		t.Fatalf("review reject: %v", err)
	}
	got := f.findActivity(a.ID)
	_ = got
	var reg model.Registration
	if err := f.db.First(&reg, erinReg.ID).Error; err != nil {
		t.Fatalf("reload rejected registration: %v", err)
	}
	if reg.Status != constants.RegistrationStatusRegistered || reg.ReviewStatus != constants.ReviewStatusRejected {
		t.Fatalf("rejected registration should stay status=registered, review_status=rejected, got status=%s review=%s",
			reg.Status, reg.ReviewStatus)
	}

	// 三类有效报名者（已通过、待审核、已签到）。
	f.seedRegistration(a.ID, userAP, constants.RegistrationStatusRegistered, constants.ReviewStatusApproved)
	f.seedRegistration(a.ID, userBP, constants.RegistrationStatusRegistered, constants.ReviewStatusPending)
	checkedIn := f.seedRegistration(a.ID, userCC, constants.RegistrationStatusRegistered, constants.ReviewStatusApproved)
	checkedIn.Status = constants.RegistrationStatusCheckedIn
	if err := f.db.Save(checkedIn).Error; err != nil {
		t.Fatalf("mark checked in: %v", err)
	}

	// 拒绝后再编辑标题/开始时间/结束时间/地点/名额（缺陷触发点）。
	newStart := a.StartTime.Add(6 * time.Hour)
	if _, err := f.activitySvc.Update(a.ID, orgID, constants.RoleOrganizer, map[string]any{
		"title":      "拒绝后改的标题",
		"start_time": newStart,
		"end_time":   newStart.Add(2 * time.Hour),
		"location":   "拒绝后改的地点",
		"capacity":   66,
	}); err != nil {
		t.Fatalf("update after rejection: %v", err)
	}

	// 被拒绝者：零变更提醒。
	if notes := f.listChangeNotifications(userRJ); len(notes) != 0 {
		t.Fatalf("rejected registration must not receive change notifications, got %d", len(notes))
	}
	// 被拒绝者既有的报名成功 + 审核结果通知仍可正常回读，不受影响。
	list, total, err := f.notifySvc.ListMine(userRJ, 1, 100)
	if err != nil {
		t.Fatalf("list erin notifications: %v", err)
	}
	if total != 2 {
		t.Fatalf("rejected user should retain 2 flow notifications (signup+review), got %d", total)
	}
	types := map[string]bool{}
	for _, item := range list {
		types[item.(map[string]any)["notification_type"].(string)] = true
	}
	if !types[constants.NotificationSignupSuccess] || !types[constants.NotificationReviewResult] {
		t.Fatalf("retained notifications should be signup_success + review_result, got %v", types)
	}

	// 三类有效报名者各收到恰好一条，且内容含全部五组旧值/新值。
	for idx, uid := range validChangeRecipients {
		notes := f.listChangeNotifications(uid)
		if len(notes) != 1 {
			t.Fatalf("valid recipient %d (index %d, order 已报名→待审核→已签到) expected 1 notification, got %d", uid, idx, len(notes))
		}
		content := notes[0]["content"].(string)
		for _, want := range []string{"标题由「", "开始时间由「", "结束时间由「", "地点由「", "名额由「", "100", "66"} {
			if !strings.Contains(content, want) {
				t.Errorf("recipient %d: content %q missing %q", uid, content, want)
			}
		}
	}

	// 再连续编辑一次（仅地点）：被拒绝者仍为 0，三类有效者各累计 2 条。
	if _, err := f.activitySvc.Update(a.ID, orgID, constants.RoleOrganizer, map[string]any{
		"location": "第二次改地点",
	}); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if notes := f.listChangeNotifications(userRJ); len(notes) != 0 {
		t.Fatalf("rejected registration must not receive the 2nd change notification either, got %d", len(notes))
	}
	if total := f.notificationCount(userRJ); total != 2 {
		t.Fatalf("rejected user notification count must stay 2, got %d", total)
	}
	for _, uid := range validChangeRecipients {
		if notes := f.listChangeNotifications(uid); len(notes) != 2 {
			t.Fatalf("valid recipient %d expected 2 notifications after 2 edits, got %d", uid, len(notes))
		}
	}
}

// 场景 4：只改无关字段（描述/封面/类型/报名截止）不发提醒。
func TestActivityChange_UnrelatedFieldsDoNotNotify(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	a := f.seedActivity(103, constants.ActivityStatusPublished)
	f.seedAllKindsOfRegistrations(103)

	if _, err := f.activitySvc.Update(103, orgID, constants.RoleOrganizer, map[string]any{
		"description":     "全新的描述内容",
		"cover_image":     "/uploads/new-cover.png",
		"activity_type":   constants.ActivityTypeTraining,
		"signup_deadline": a.SignupDeadline.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("update unrelated fields: %v", err)
	}
	for _, uid := range []uint64{userAP, userBP, userCC, userXX, userRJ} {
		if n := f.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("unrelated-field edit must not notify user %d, got %d", uid, len(n))
		}
	}
}

// 场景 5：关键字段提交但值未变（幂等保存）不发提醒。
func TestActivityChange_UnchangedKeyFieldsDoNotNotify(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	a := f.seedActivity(104, constants.ActivityStatusPublished)
	f.seedAllKindsOfRegistrations(104)

	if _, err := f.activitySvc.Update(104, orgID, constants.RoleOrganizer, map[string]any{
		"title":      a.Title,
		"start_time": a.StartTime,
		"end_time":   a.EndTime,
		"location":   a.Location,
		"capacity":   a.Capacity,
	}); err != nil {
		t.Fatalf("save unchanged: %v", err)
	}
	for _, uid := range validChangeRecipients {
		if n := f.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("saving unchanged key fields must not notify user %d, got %d", uid, len(n))
		}
	}
}

// 场景 6：连续编辑按“当次变化”各生成一条；第二次只含当次字段。
func TestActivityChange_ConsecutiveEditsReflectEachDelta(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	f.seedActivity(105, constants.ActivityStatusPublished)
	f.seedAllKindsOfRegistrations(105)

	// 第一次：标题 + 地点。
	if _, err := f.activitySvc.Update(105, orgID, constants.RoleOrganizer, map[string]any{
		"title": "第二次标题", "location": "第二次地点",
	}); err != nil {
		t.Fatalf("edit 1: %v", err)
	}
	// 第二次：仅名额。
	if _, err := f.activitySvc.Update(105, orgID, constants.RoleOrganizer, map[string]any{
		"capacity": 30,
	}); err != nil {
		t.Fatalf("edit 2: %v", err)
	}
	// 第三次：仅开始时间。
	a := f.findActivity(105)
	if _, err := f.activitySvc.Update(105, orgID, constants.RoleOrganizer, map[string]any{
		"start_time": a.StartTime.Add(12 * time.Hour),
	}); err != nil {
		t.Fatalf("edit 3: %v", err)
	}

	for _, uid := range validChangeRecipients {
		notes := f.listChangeNotifications(uid)
		if len(notes) != 3 {
			t.Fatalf("user %d: expected 3 notifications (one per edit), got %d", uid, len(notes))
		}
		// 回读顺序为 created_at DESC, id DESC：首条对应第三次（仅开始时间）。
		latest := notes[0]["content"].(string)
		if !strings.Contains(latest, "开始时间由「") {
			t.Errorf("user %d: 3rd notification must mention 开始时间: %s", uid, latest)
		}
		for _, other := range []string{"标题由「", "地点由「", "名额由「", "结束时间由「"} {
			if strings.Contains(latest, other) {
				t.Errorf("user %d: 3rd notification must only carry 开始时间, found %s: %s", uid, other, latest)
			}
		}
		// 第二条对应第二次（仅名额）。
		middle := notes[1]["content"].(string)
		if !strings.Contains(middle, "名额由「") {
			t.Errorf("user %d: 2nd notification must carry 名额: %s", uid, middle)
		}
		for _, other := range []string{"标题由「", "地点由「", "开始时间由「", "结束时间由「"} {
			if strings.Contains(middle, other) {
				t.Errorf("user %d: 2nd notification must only carry 名额, found %s: %s", uid, other, middle)
			}
		}
		// 第一条对应第一次（标题 + 地点）。
		first := notes[2]["content"].(string)
		if !strings.Contains(first, "标题由「") || !strings.Contains(first, "地点由「") {
			t.Errorf("user %d: 1st notification must carry 标题 and 地点: %s", uid, first)
		}
		if strings.Contains(first, "名额由「") || strings.Contains(first, "开始时间由「") || strings.Contains(first, "结束时间由「") {
			t.Errorf("user %d: 1st notification must only carry 标题 and 地点: %s", uid, first)
		}
	}
}

// 场景 7：越权编辑既不修改活动也不发送提醒。
func TestActivityChange_UnauthorizedEditChangesNothing(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	a := f.seedActivity(106, constants.ActivityStatusPublished)
	f.seedAllKindsOfRegistrations(106)

	_, err := f.activitySvc.Update(106, userAP, constants.RoleUser, map[string]any{
		"title": "黑客改标题", "location": "黑客改地点", "capacity": 1,
	})
	if err == nil {
		t.Fatal("non-organizer update must return an error")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error should be forbidden, got: %v", err)
	}

	// 活动数据保持原样。
	got := f.findActivity(106)
	if got.Title != a.Title || got.Location != a.Location || got.Capacity != a.Capacity ||
		!got.StartTime.Equal(a.StartTime) || !got.EndTime.Equal(a.EndTime) {
		t.Fatalf("activity must be unchanged after forbidden edit: %+v", got)
	}
	// 不产生任何通知（含操作者本人与其他报名者）。
	for _, uid := range []uint64{userAP, userBP, userCC, userXX, userRJ} {
		if n := f.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("forbidden edit must not notify user %d, got %d", uid, len(n))
		}
	}
}

// 场景 8：管理员可编辑他人活动并正常提醒。
func TestActivityChange_AdminCanEditAndNotify(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	f.seedActivity(107, constants.ActivityStatusPublished)
	f.seedAllKindsOfRegistrations(107)

	if _, err := f.activitySvc.Update(107, 999, constants.RoleAdmin, map[string]any{
		"title": "管理员改的标题",
	}); err != nil {
		t.Fatalf("admin update: %v", err)
	}
	for _, uid := range validChangeRecipients {
		if n := f.listChangeNotifications(uid); len(n) != 1 {
			t.Fatalf("admin edit should notify valid registrant %d, got %d", uid, len(n))
		}
	}
}

// 场景 9：通知写入后可回读，单条与全部已读状态可正常更新。
func TestActivityChange_ReadStateUpdates(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	f.seedActivity(108, constants.ActivityStatusPublished)
	f.seedAllKindsOfRegistrations(108)

	if _, err := f.activitySvc.Update(108, orgID, constants.RoleOrganizer, map[string]any{
		"title": "已读测试标题",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// 单条已读。
	bob := f.listChangeNotifications(userBP)
	if len(bob) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(bob))
	}
	id := bob[0]["id"].(uint64)
	if err := f.notifySvc.MarkRead(id, userBP); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if after := f.listChangeNotifications(userBP); !after[0]["is_read"].(bool) {
		t.Fatal("notification should be read after MarkRead")
	}
	// 不能把别人的通知标为已读（where user_id 限制）：Carol 的仍未读。
	carol := f.listChangeNotifications(userCC)
	if carol[0]["is_read"].(bool) {
		t.Fatal("marking one user's notification must not affect another user")
	}

	// 全部已读。
	if err := f.notifySvc.MarkAllRead(userCC); err != nil {
		t.Fatalf("mark all read: %v", err)
	}
	if after := f.listChangeNotifications(userCC); !after[0]["is_read"].(bool) {
		t.Fatal("notification should be read after MarkAllRead")
	}
}

// 场景 10（回归）：发布、在线报名、审核、凭证签到、名单导出原有流程保持可用，
// 且发布/结束动作本身不产生变更提醒。
func TestActivityChange_Regression_CoreFlowsStillWork(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()

	// 1) 组织者创建草稿。
	a, err := f.activitySvc.Create(orgID, "回归活动", "描述", "", constants.ActivityTypeLecture, "会场",
		time.Now().Add(72*time.Hour), time.Now().Add(74*time.Hour), time.Now().Add(70*time.Hour),
		100, constants.ActivityStatusDraft)
	if err != nil {
		t.Fatalf("create draft: %v", err)
	}

	// 草稿阶段用户报名应失败（活动未发布）。
	if _, err := f.regSvc.Create(a.ID, userAP, "Alice", "13900000001", ""); err == nil {
		t.Fatal("signup on draft activity must fail")
	}

	// 2) 发布。
	pub, err := f.activitySvc.Publish(a.ID, orgID, constants.RoleOrganizer)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.Status != constants.ActivityStatusPublished {
		t.Fatalf("status = %s, want published", pub.Status)
	}

	// 3) Alice 在线报名（事务：名额校验、防重、凭证号、报名成功通知）。
	reg, err := f.regSvc.Create(a.ID, userAP, "Alice", "13900000001", "")
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	if reg.VoucherNo == "" {
		t.Fatal("voucher number should be generated")
	}
	// 防重复报名。
	if _, err := f.regSvc.Create(a.ID, userAP, "Alice", "13900000001", ""); err == nil {
		t.Fatal("duplicate signup must fail")
	}

	// 4) Bob 报名并被组织者审核通过（pending -> approved）。
	bobReg, err := f.regSvc.Create(a.ID, userBP, "Bob", "13900000002", "")
	if err != nil {
		t.Fatalf("bob signup: %v", err)
	}
	if _, err := f.regSvc.Review(bobReg.ID, orgID, constants.RoleOrganizer, constants.ReviewStatusApproved); err != nil {
		t.Fatalf("review approve: %v", err)
	}
	// 重复审核应失败。
	if _, err := f.regSvc.Review(bobReg.ID, orgID, constants.RoleOrganizer, constants.ReviewStatusRejected); err == nil {
		t.Fatal("re-review must fail")
	}

	// 5) Alice 凭证号签到（registered -> checked_in + 签到成功通知）。
	rec, err := f.checkinSvc.CheckInByVoucher(a.ID, orgID, reg.VoucherNo)
	if err != nil {
		t.Fatalf("checkin by voucher: %v", err)
	}
	if rec.CheckInMethod != constants.CheckInMethodVoucher {
		t.Fatalf("checkin method = %s", rec.CheckInMethod)
	}
	// 重复签到应失败。
	if _, err := f.checkinSvc.CheckInByVoucher(a.ID, orgID, reg.VoucherNo); err == nil {
		t.Fatal("duplicate checkin must fail")
	}

	// 通知计数回归：Alice 报名成功 + 签到成功 = 2；Bob 报名成功 + 审核结果 = 2；均无变更提醒。
	if total := f.notificationCount(userAP); total != 2 {
		t.Errorf("alice notifications = %d, want 2 (signup+checkin)", total)
	}
	if total := f.notificationCount(userBP); total != 2 {
		t.Errorf("bob notifications = %d, want 2 (signup+review)", total)
	}
	for _, uid := range []uint64{userAP, userBP} {
		if n := f.listChangeNotifications(uid); len(n) != 0 {
			t.Fatalf("publish/signup/review/checkin must not create change notifications for %d, got %d", uid, len(n))
		}
	}

	// 6) 名单导出 CSV 回归：包含两位报名者与凭证号。
	filename, csvText, err := f.regSvc.ExportCSV(a.ID, orgID, constants.RoleOrganizer)
	if err != nil {
		t.Fatalf("export csv: %v", err)
	}
	if !strings.HasPrefix(filename, "registrations_") || !strings.HasSuffix(filename, ".csv") {
		t.Errorf("unexpected csv filename: %s", filename)
	}
	if !strings.Contains(csvText, reg.VoucherNo) || !strings.Contains(csvText, bobReg.VoucherNo) {
		t.Errorf("csv should contain both voucher numbers:\n%s", csvText)
	}
	// 非组织者导出应被拒绝。
	if _, _, err := f.regSvc.ExportCSV(a.ID, userAP, constants.RoleUser); err == nil {
		t.Fatal("non-organizer export must fail")
	}

	// 7) 活动统计回归（报名/签到人数）。
	stats, err := f.activitySvc.Stats(a.ID, orgID, constants.RoleOrganizer)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats["registered_count"].(int64) != 2 {
		t.Errorf("registered_count = %v, want 2", stats["registered_count"])
	}
	if stats["checked_in_count"].(int64) != 1 {
		t.Errorf("checked_in_count = %v, want 1", stats["checked_in_count"])
	}

	// 7.5) 取消流程回归：Carol 报名后取消（registered -> cancelled），
	// 取消本身不产生通知；随后编辑关键字段，她不进入提醒名单，也不影响其他有效者。
	carolReg, err := f.regSvc.Create(a.ID, userCC, "Carol", "13900000003", "")
	if err != nil {
		t.Fatalf("carol signup: %v", err)
	}
	canceled, err := f.regSvc.Cancel(carolReg.ID, userCC, constants.RoleUser)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if canceled.Status != constants.RegistrationStatusCancelled {
		t.Fatalf("status = %s, want cancelled", canceled.Status)
	}
	// 已取消后再取消应冲突。
	if _, err := f.regSvc.Cancel(carolReg.ID, userCC, constants.RoleUser); err == nil {
		t.Fatal("cancelling twice must fail")
	}
	carolSignupNotices := f.notificationCount(userCC) // 取消前的报名成功通知仍保留可回读
	if carolSignupNotices != 1 {
		t.Fatalf("carol should retain her signup notification for readback, got %d", carolSignupNotices)
	}
	if _, err := f.activitySvc.Update(a.ID, orgID, constants.RoleOrganizer, map[string]any{
		"title": "取消之后改标题",
	}); err != nil {
		t.Fatalf("update after cancel: %v", err)
	}
	if notes := f.listChangeNotifications(userCC); len(notes) != 0 {
		t.Fatalf("cancelled registrant must not be notified after later edit, got %d", len(notes))
	}
	if total := f.notificationCount(userCC); total != carolSignupNotices {
		t.Fatalf("cancelled registrant notifications must stay at %d (readback intact), got %d", carolSignupNotices, total)
	}
	for _, uid := range []uint64{userAP, userBP} {
		if notes := f.listChangeNotifications(uid); len(notes) != 1 {
			t.Fatalf("valid registrant %d should still receive exactly 1 change notification, got %d", uid, len(notes))
		}
	}

	// 8) 结束活动，结束动作不产生变更提醒。
	ended, err := f.activitySvc.End(a.ID, orgID, constants.RoleOrganizer)
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if ended.Status != constants.ActivityStatusEnded {
		t.Fatalf("status = %s, want ended", ended.Status)
	}
	for _, uid := range []uint64{userAP, userBP} {
		if notes := f.listChangeNotifications(uid); len(notes) != 1 {
			t.Fatalf("end must not create additional change notifications for %d, got %d", uid, len(notes))
		}
	}
}

// 场景 11（回归）：评论与收藏在引入变更提醒后仍正常工作。
func TestActivityChange_Regression_CommentAndFavorite(t *testing.T) {
	f := newChangeFixture(t)
	f.seedUsers()
	a := f.seedActivity(109, constants.ActivityStatusPublished)

	if _, err := f.commentSvc.Create(a.ID, userAP, 5, "很赞的活动"); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	list, err := f.commentSvc.ListByActivity(a.ID)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("comment count = %d, want 1", len(list))
	}

	if _, err := f.favoriteSvc.Add(userBP, a.ID); err != nil {
		t.Fatalf("add favorite: %v", err)
	}
	favs, _, err := f.favoriteSvc.ListMine(userBP, 1, 10)
	if err != nil {
		t.Fatalf("list favorites: %v", err)
	}
	if len(favs) != 1 {
		t.Fatalf("favorite count = %d, want 1", len(favs))
	}
}

// 兜底确认：更新不存在的活动返回仓储哨兵错误（依赖该错误判断的既有逻辑不受影响）。
func TestActivityChange_NotFoundSentinel(t *testing.T) {
	f := newChangeFixture(t)
	_, err := f.activitySvc.Update(999999, orgID, constants.RoleOrganizer, map[string]any{"title": "x"})
	if err == nil {
		t.Fatal("updating missing activity must fail")
	}
	if !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("error should wrap repository.ErrNotFound, got: %v", err)
	}
}
