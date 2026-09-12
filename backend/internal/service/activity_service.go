package service

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gbevent/internal/constants"
	"gbevent/internal/model"
	"gbevent/internal/repository"
	"gbevent/internal/util"

	"gorm.io/gorm"
)

// ActivityService 活动业务逻辑。
type ActivityService struct {
	db          *gorm.DB
	repo        *repository.ActivityRepository
	regRepo     *repository.RegistrationRepository
	notifyRepo  *repository.NotificationRepository
	checkinRepo *repository.CheckInRecordRepository
	logger      *slog.Logger
}

// NewActivityService 构造活动服务。
func NewActivityService(db *gorm.DB, repo *repository.ActivityRepository, regRepo *repository.RegistrationRepository,
	notifyRepo *repository.NotificationRepository, checkinRepo *repository.CheckInRecordRepository,
	logger *slog.Logger) *ActivityService {
	return &ActivityService{db: db, repo: repo, regRepo: regRepo, notifyRepo: notifyRepo, checkinRepo: checkinRepo, logger: logger}
}

// Create 创建活动。
func (s *ActivityService) Create(organizerID uint64, title, description, coverImage, activityType, location string,
	startTime, endTime, signupDeadline time.Time, capacity int, status string) (*model.Activity, error) {
	if !constants.IsValidActivityType(activityType) {
		return nil, util.NewAppError(constants.CodeValidationFailed, "Activity[activity_type="+activityType+"] create: invalid type")
	}
	if status == "" {
		status = constants.ActivityStatusDraft
	}
	if !constants.IsValidActivityStatus(status) {
		return nil, util.NewAppError(constants.CodeValidationFailed, "Activity[status="+status+"] create: invalid status")
	}
	a := &model.Activity{
		Title:          title,
		Description:    description,
		CoverImage:     coverImage,
		ActivityType:   activityType,
		StartTime:      startTime,
		EndTime:        endTime,
		Location:       location,
		Capacity:       capacity,
		SignupDeadline: signupDeadline,
		Status:         status,
		OrganizerID:    organizerID,
	}
	if err := s.repo.Create(a); err != nil {
		s.logger.Error(constants.LogActivityCreateFailed, "error", err)
		return nil, util.Wrap(err, "Activity[organizer_id=%d] create failed", organizerID)
	}
	s.logger.Info(constants.LogActivityCreateSuccess, "activity_id", a.ID)
	return a, nil
}

// Update 更新活动（仅发布者或管理员）。
// 已发布活动的 title/start_time/end_time/location/capacity 变更后，会向每位有效报名者
// （status != cancelled）合并发送一条 activity_change 未读通知；草稿保存或仅修改无关字段不发送。
func (s *ActivityService) Update(id, operatorID uint64, operatorRole string, fields map[string]any) (*model.Activity, error) {
	var a *model.Activity
	var changed []activityFieldChange
	err := s.db.Transaction(func(tx *gorm.DB) error {
		cur, err := s.repo.FindByIDForUpdate(tx, id)
		if err != nil {
			return util.Wrap(err, "Activity[id=%d] update find failed", id)
		}
		if operatorRole != constants.RoleAdmin && cur.OrganizerID != operatorID {
			return util.NewAppError(constants.CodeForbidden, "Activity[id="+itoa(id)+"] update forbidden: organizer not match")
		}

		// 保存关键字段旧值，更新后再比对生成变更明细。
		old := *cur

		if v, ok := fields["title"].(string); ok && v != "" {
			cur.Title = v
		}
		if v, ok := fields["description"].(string); ok {
			cur.Description = v
		}
		if v, ok := fields["cover_image"].(string); ok {
			cur.CoverImage = v
		}
		if v, ok := fields["activity_type"].(string); ok && v != "" {
			if !constants.IsValidActivityType(v) {
				return util.NewAppError(constants.CodeValidationFailed, "Activity[id="+itoa(id)+"] update: invalid activity_type="+v)
			}
			cur.ActivityType = v
		}
		if v, ok := fields["start_time"].(time.Time); ok {
			cur.StartTime = v.In(time.Local)
		}
		if v, ok := fields["end_time"].(time.Time); ok {
			cur.EndTime = v.In(time.Local)
		}
		if v, ok := fields["signup_deadline"].(time.Time); ok {
			cur.SignupDeadline = v.In(time.Local)
		}
		if v, ok := fields["location"].(string); ok {
			cur.Location = v
		}
		if v, ok := fields["capacity"].(int); ok {
			cur.Capacity = v
		}

		if err := s.repo.UpdateTx(tx, cur); err != nil {
			return util.Wrap(err, "Activity[id=%d] update save failed", id)
		}

		// 仅已发布活动需要通知；草稿编辑、发布/结束动作各自的流程不产生变更提醒。
		if cur.Status == constants.ActivityStatusPublished {
			changed = buildActivityFieldChanges(&old, cur)
			if len(changed) > 0 {
				if err := s.notifyActivityChangeTx(tx, cur, changed); err != nil {
					return util.Wrap(err, "Activity[id=%d] update notify failed", id)
				}
			}
		}
		a = cur
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info(constants.LogActivityUpdateSuccess, "activity_id", a.ID, "changed_fields", len(changed))
	return a, nil
}

// activityFieldChange 单个关键信息字段的旧值/新值。
type activityFieldChange struct {
	label string
	oldV  string
	newV  string
}

// buildActivityFieldChanges 比对关键字段，返回实际发生变化的字段列表。
func buildActivityFieldChanges(old, cur *model.Activity) []activityFieldChange {
	changes := make([]activityFieldChange, 0, 5)
	if old.Title != cur.Title {
		changes = append(changes, activityFieldChange{"标题", old.Title, cur.Title})
	}
	if !old.StartTime.Equal(cur.StartTime) {
		changes = append(changes, activityFieldChange{"开始时间", util.FormatDateTime(old.StartTime), util.FormatDateTime(cur.StartTime)})
	}
	if !old.EndTime.Equal(cur.EndTime) {
		changes = append(changes, activityFieldChange{"结束时间", util.FormatDateTime(old.EndTime), util.FormatDateTime(cur.EndTime)})
	}
	if old.Location != cur.Location {
		changes = append(changes, activityFieldChange{"地点", old.Location, cur.Location})
	}
	if old.Capacity != cur.Capacity {
		changes = append(changes, activityFieldChange{"名额", formatCapacityText(old.Capacity), formatCapacityText(cur.Capacity)})
	}
	return changes
}

// formatCapacityText 名额展示，0 表示不限。
func formatCapacityText(capacity int) string {
	if capacity <= 0 {
		return "不限"
	}
	return fmt.Sprintf("%d", capacity)
}

// renderActivityChangeContent 将多个字段变更合并为一条通知正文。
func renderActivityChangeContent(activityTitle string, changes []activityFieldChange) string {
	var b strings.Builder
	b.WriteString("您报名的活动《")
	b.WriteString(activityTitle)
	b.WriteString("》关键信息发生变更：")
	for i, ch := range changes {
		if i > 0 {
			b.WriteString("；")
		}
		b.WriteString(ch.label)
		b.WriteString("由「")
		b.WriteString(ch.oldV)
		b.WriteString("」变更为「")
		b.WriteString(ch.newV)
		b.WriteString("」")
	}
	b.WriteString("。请留意最新安排。")
	return b.String()
}

// notifyActivityChangeTx 在事务内向每位有效报名者写入同一条未读变更提醒。
func (s *ActivityService) notifyActivityChangeTx(tx *gorm.DB, a *model.Activity, changes []activityFieldChange) error {
	userIDs, err := s.regRepo.ListValidUserIDsByActivityTx(tx, a.ID)
	if err != nil {
		return util.Wrap(err, "Activity[id=%d] list valid registrations failed", a.ID)
	}
	if len(userIDs) == 0 {
		return nil
	}
	s.logger.Info(constants.LogActivityChangeNotifyStart, "activity_id", a.ID, "recipients", len(userIDs), "changed_fields", len(changes))
	content := renderActivityChangeContent(a.Title, changes)
	notifications := make([]*model.Notification, 0, len(userIDs))
	for _, uid := range userIDs {
		notifications = append(notifications, &model.Notification{
			UserID:           uid,
			NotificationType: constants.NotificationActivityChange,
			Title:            constants.MsgActivityChanged,
			Content:          content,
		})
	}
	if err := s.notifyRepo.BatchCreateTx(tx, notifications); err != nil {
		s.logger.Error(constants.LogActivityChangeNotifyFailed, "activity_id", a.ID, "error", err)
		return util.Wrap(err, "Activity[id=%d] change notifications create failed", a.ID)
	}
	s.logger.Info(constants.LogActivityChangeNotifySuccess, "activity_id", a.ID, "recipients", len(userIDs))
	return nil
}

// Publish 发布活动（draft -> published）。
func (s *ActivityService) Publish(id, operatorID uint64, operatorRole string) (*model.Activity, error) {
	a, err := s.repo.FindByID(id)
	if err != nil {
		return nil, util.Wrap(err, "Activity[id=%d] publish find failed", id)
	}
	if operatorRole != constants.RoleAdmin && a.OrganizerID != operatorID {
		return nil, util.NewAppError(constants.CodeForbidden, "Activity[id="+itoa(id)+"] publish forbidden: organizer not match")
	}
	if a.Status != constants.ActivityStatusDraft {
		return nil, util.NewAppError(constants.CodeConflict, "Activity[id="+itoa(id)+"] publish conflict: status="+a.Status)
	}
	a.Status = constants.ActivityStatusPublished
	if err := s.repo.Update(a); err != nil {
		return nil, util.Wrap(err, "Activity[id=%d] publish save failed", id)
	}
	s.logger.Info(constants.LogActivityPublishSuccess, "activity_id", a.ID)
	return a, nil
}

// End 结束活动（published -> ended）。
func (s *ActivityService) End(id, operatorID uint64, operatorRole string) (*model.Activity, error) {
	a, err := s.repo.FindByID(id)
	if err != nil {
		return nil, util.Wrap(err, "Activity[id=%d] end find failed", id)
	}
	if operatorRole != constants.RoleAdmin && a.OrganizerID != operatorID {
		return nil, util.NewAppError(constants.CodeForbidden, "Activity[id="+itoa(id)+"] end forbidden: organizer not match")
	}
	if a.Status != constants.ActivityStatusPublished {
		return nil, util.NewAppError(constants.CodeConflict, "Activity[id="+itoa(id)+"] end conflict: status="+a.Status)
	}
	a.Status = constants.ActivityStatusEnded
	if err := s.repo.Update(a); err != nil {
		return nil, util.Wrap(err, "Activity[id=%d] end save failed", id)
	}
	s.logger.Info(constants.LogActivityEndSuccess, "activity_id", a.ID)
	return a, nil
}

// Delete 删除活动（仅发布者或管理员）。
func (s *ActivityService) Delete(id, operatorID uint64, operatorRole string) error {
	a, err := s.repo.FindByID(id)
	if err != nil {
		return util.Wrap(err, "Activity[id=%d] delete find failed", id)
	}
	if operatorRole != constants.RoleAdmin && a.OrganizerID != operatorID {
		return util.NewAppError(constants.CodeForbidden, "Activity[id="+itoa(id)+"] delete forbidden: organizer not match")
	}
	if err := s.repo.Delete(id); err != nil {
		return util.Wrap(err, "Activity[id=%d] delete failed", id)
	}
	s.logger.Info(constants.LogActivityDeleteSuccess, "activity_id", id)
	return nil
}

// List 分页查询活动。
func (s *ActivityService) List(page, pageSize int, activityType, status, keyword string) ([]model.Activity, int64, error) {
	return s.repo.List(page, pageSize, activityType, status, keyword)
}

// ListByOrganizer 查询组织者自己的活动。
func (s *ActivityService) ListByOrganizer(organizerID uint64, page, pageSize int, status string) ([]model.Activity, int64, error) {
	return s.repo.ListByOrganizer(organizerID, page, pageSize, status)
}

// Get 查询活动详情，附带报名人数与签到人数。
func (s *ActivityService) Get(id uint64) (*model.Activity, int64, error) {
	a, err := s.repo.FindByID(id)
	if err != nil {
		return nil, 0, util.Wrap(err, "Activity[id=%d] get failed", id)
	}
	count, err := s.repo.CountRegistered(id)
	if err != nil {
		return nil, 0, util.Wrap(err, "Activity[id=%d] count registered failed", id)
	}
	return a, count, nil
}

// Calendar 按月份返回活动日历数据。
func (s *ActivityService) Calendar(month string) ([]model.Activity, error) {
	start, err := time.Parse("2006-01", month)
	if err != nil {
		start = time.Now()
	}
	start = time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.Local)
	end := start.AddDate(0, 1, 0)
	list, err := s.repo.CalendarCounts(start, end)
	if err != nil {
		return nil, util.Wrap(err, "Activity[month=%s] calendar failed", month)
	}
	return list, nil
}

// Stats 统计活动报名/签到情况。
func (s *ActivityService) Stats(activityID, operatorID uint64, operatorRole string) (map[string]any, error) {
	a, err := s.repo.FindByID(activityID)
	if err != nil {
		return nil, util.Wrap(err, "Activity[id=%d] stats find failed", activityID)
	}
	if operatorRole != constants.RoleAdmin && a.OrganizerID != operatorID {
		return nil, util.NewAppError(constants.CodeForbidden, "Activity[id="+itoa(activityID)+"] stats forbidden: organizer not match")
	}
	registered, err := s.repo.CountRegistered(activityID)
	if err != nil {
		return nil, err
	}
	checked, err := s.checkinRepo.CountCheckedIn(activityID)
	if err != nil {
		return nil, err
	}
	rate := 0.0
	if registered > 0 {
		rate = float64(checked) / float64(registered) * 100
	}
	s.logger.Info(constants.LogCheckinRateStats, "activity_id", activityID, "rate", rate)
	return map[string]any{
		"activity_id":      activityID,
		"capacity":         a.Capacity,
		"registered_count": registered,
		"checked_in_count": checked,
		"checkin_rate":     round2(rate),
	}, nil
}

// CheckRegistrationLimit 校验报名名额与截止时间（供 RegistrationService 使用）。
func (s *ActivityService) CheckRegistrationLimit(activityID uint64) error {
	a, err := s.repo.FindByID(activityID)
	if err != nil {
		return util.Wrap(err, "Activity[id=%d] check limit failed", activityID)
	}
	return s.checkRegistrationLimit(a, func(activityID uint64) (int64, error) {
		return s.repo.CountRegistered(activityID)
	})
}

// CheckRegistrationLimitTx 在事务内校验报名名额与截止时间。
func (s *ActivityService) CheckRegistrationLimitTx(tx *gorm.DB, activityID uint64) error {
	a, err := s.repo.FindByIDForUpdate(tx, activityID)
	if err != nil {
		return util.Wrap(err, "Activity[id=%d] check limit failed", activityID)
	}
	return s.checkRegistrationLimit(a, func(activityID uint64) (int64, error) {
		return s.repo.CountRegisteredTx(tx, activityID)
	})
}

func (s *ActivityService) checkRegistrationLimit(a *model.Activity, countFn func(uint64) (int64, error)) error {
	if a.Status == constants.ActivityStatusEnded {
		return util.NewAppError(constants.CodeActivityEnded, constants.MsgActivityEnded)
	}
	if a.Status != constants.ActivityStatusPublished {
		return util.NewAppError(constants.CodeConflict, "Activity[id="+itoa(a.ID)+"] not published")
	}
	if time.Now().After(a.SignupDeadline) {
		return util.NewAppError(constants.CodeActivityEnded, "Activity[id="+itoa(a.ID)+"] signup deadline passed")
	}
	count, err := countFn(a.ID)
	if err != nil {
		return err
	}
	if a.Capacity > 0 && count >= int64(a.Capacity) {
		return util.NewAppError(constants.CodeActivityFull, constants.MsgActivityFull)
	}
	return nil
}

// CreateSignupNotification 生成报名/审核/签到通知。
func (s *ActivityService) CreateSignupNotification(userID uint64, notifType, title, content string) error {
	n := &model.Notification{UserID: userID, NotificationType: notifType, Title: title, Content: content}
	if err := s.notifyRepo.Create(n); err != nil {
		s.logger.Error(constants.LogNotificationCreate, "error", err)
		return util.Wrap(err, "Notification[user_id=%d] create failed", userID)
	}
	s.logger.Info(constants.LogNotificationCreate, "user_id", userID, "type", notifType)
	return nil
}

// CreateSignupNotificationTx 在事务内生成报名/审核/签到通知。
func (s *ActivityService) CreateSignupNotificationTx(tx *gorm.DB, userID uint64, notifType, title, content string) error {
	n := &model.Notification{UserID: userID, NotificationType: notifType, Title: title, Content: content}
	if err := s.notifyRepo.CreateTx(tx, n); err != nil {
		s.logger.Error(constants.LogNotificationCreate, "error", err)
		return util.Wrap(err, "Notification[user_id=%d] create failed", userID)
	}
	s.logger.Info(constants.LogNotificationCreate, "user_id", userID, "type", notifType)
	return nil
}

// IsOrganizer 判断操作者是否活动组织者或管理员。
func IsOrganizer(operatorID uint64, operatorRole string, organizerID uint64) bool {
	return operatorRole == constants.RoleAdmin || operatorID == organizerID
}

// errNotFound 判断是否为未找到错误。
func errNotFound(err error) bool {
	return errors.Is(err, repository.ErrNotFound)
}

func itoa(v uint64) string {
	return fmtUint(v)
}

func fmtUint(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
