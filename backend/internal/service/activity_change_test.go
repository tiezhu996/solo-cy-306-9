package service

import (
	"strings"
	"testing"
	"time"

	"gbevent/internal/constants"
	"gbevent/internal/model"
)

func newChangeTestActivity() *model.Activity {
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)
	return &model.Activity{
		ID:        10,
		Title:     "原标题",
		StartTime: base,
		EndTime:   base.Add(2 * time.Hour),
		Location:  "A 座 3 楼",
		Capacity:  100,
		Status:    constants.ActivityStatusPublished,
	}
}

func TestBuildActivityFieldChanges(t *testing.T) {
	t.Run("all five key fields", func(t *testing.T) {
		old := newChangeTestActivity()
		cur := newChangeTestActivity()
		cur.Title = "新标题"
		cur.StartTime = old.StartTime.Add(24 * time.Hour)
		cur.EndTime = old.EndTime.Add(48 * time.Hour)
		cur.Location = "B 座 5 楼"
		cur.Capacity = 200

		changes := buildActivityFieldChanges(old, cur)
		if len(changes) != 5 {
			t.Fatalf("expected 5 changes, got %d: %+v", len(changes), changes)
		}
		labels := map[string]bool{}
		for _, ch := range changes {
			labels[ch.label] = true
			if ch.oldV == "" || ch.newV == "" {
				t.Errorf("change %q should carry old and new values: %+v", ch.label, ch)
			}
		}
		for _, want := range []string{"标题", "开始时间", "结束时间", "地点", "名额"} {
			if !labels[want] {
				t.Errorf("missing change for field %s", want)
			}
		}
	})

	t.Run("no change returns empty", func(t *testing.T) {
		old := newChangeTestActivity()
		cur := newChangeTestActivity()
		if changes := buildActivityFieldChanges(old, cur); len(changes) != 0 {
			t.Errorf("expected no changes, got %+v", changes)
		}
	})

	t.Run("unrelated fields do not produce changes", func(t *testing.T) {
		old := newChangeTestActivity()
		cur := newChangeTestActivity()
		cur.Description = "新的描述"
		cur.CoverImage = "/img/new.png"
		cur.ActivityType = constants.ActivityTypeTraining
		cur.SignupDeadline = old.StartTime.Add(-time.Hour)
		if changes := buildActivityFieldChanges(old, cur); len(changes) != 0 {
			t.Errorf("description/cover/type/deadline must not notify, got %+v", changes)
		}
	})

	t.Run("unlimited capacity text", func(t *testing.T) {
		if got := formatCapacityText(0); got != "不限" {
			t.Errorf("capacity 0 text = %q, want 不限", got)
		}
		if got := formatCapacityText(50); got != "50" {
			t.Errorf("capacity 50 text = %q, want 50", got)
		}
	})
}

func TestRenderActivityChangeContent(t *testing.T) {
	changes := []activityFieldChange{
		{label: "标题", oldV: "原标题", newV: "新标题"},
		{label: "地点", oldV: "A 座", newV: "B 座"},
	}
	content := renderActivityChangeContent("新标题", changes)
	if !strings.Contains(content, "原标题") || !strings.Contains(content, "新标题") {
		t.Errorf("content should include title old/new values: %s", content)
	}
	if !strings.Contains(content, "A 座") || !strings.Contains(content, "B 座") {
		t.Errorf("content should include location old/new values: %s", content)
	}
	// 多个字段必须合并在同一条正文内，只出现一次活动名前缀。
	if strings.Count(content, "关键信息发生变更") != 1 {
		t.Errorf("multiple fields must be merged into one notification body: %s", content)
	}
}
