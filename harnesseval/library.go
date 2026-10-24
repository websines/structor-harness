package harnesseval

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/websines/structor-harness/harness/improve"
	"github.com/websines/structor-harness/record"
)

func loadSeed(state string, target Manifest) ([]improve.Lesson, error) {
	if !filepath.IsAbs(state) {
		return nil, errors.New("harnesseval: absolute seed experiment required")
	}
	if _, err := os.Stat(filepath.Join(state, "cas")); err != nil {
		return nil, err
	}
	cas, err := record.NewLocalCAS(filepath.Join(state, "cas"))
	if err != nil {
		return nil, err
	}
	journal, err := improve.NewJournal(filepath.Join(state, "journal"), cas)
	if err != nil {
		return nil, err
	}
	var report Report
	if _, err = journal.Read("report", &report); err != nil {
		return nil, err
	}
	if !report.Complete || report.Manifest.Config.Split != "development" || report.Manifest.Config.Project != target.Config.Project || report.Manifest.Base.Base != target.Base.Base || report.Manifest.Dataset != target.Dataset {
		return nil, errors.New("harnesseval: seed library scope/configuration mismatch")
	}
	var lessons []improve.Lesson
	for _, trial := range report.Trials {
		var lesson improve.Lesson
		if _, err := journal.Read(trial.ID+"-lesson", &lesson); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if lesson.Scope != trial.Family || lesson.TaskID != trial.TaskID || len(lesson.Evidence) < 2 || lesson.Profile.Validate(target.Base.Base) != nil {
			return nil, errors.New("harnesseval: invalid seed lesson")
		}
		for _, d := range lesson.Evidence {
			if _, err := cas.Get(d); err != nil {
				return nil, err
			}
		}
		lessons = append(lessons, lesson)
	}
	return lessons, nil
}
