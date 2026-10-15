package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/nexustar/usher/internal/schedule"
)

// Schedule adds the next due time, which the server derives and a write never
// carries.
type Schedule struct {
	schedule.Task
	NextRun time.Time `json:"next_run,omitzero"`
}

// ScheduleList is the body of GET /schedules. The zone rides with the list: it
// is only meaningful next to these timestamps.
type ScheduleList struct {
	Schedules []Schedule `json:"schedules"`
	Timezone  string     `json:"timezone"`
}

// validateScheduleTarget rejects a task that could not create a session. The
// agent is re-resolved at fire time, so this is a courtesy, not a guarantee —
// but it catches typos that would otherwise surface at 3am as a failed run.
func (s *Server) validateScheduleTarget(task schedule.Task) error {
	opts, err := s.Agents.Resolve(task.Agent, task.Cwd, task.Backend, task.Model)
	if err != nil {
		return err
	}
	if opts.Cwd == "" {
		return errors.New("cwd is required (set one here or on the agent)")
	}
	return nil
}

func (s *Server) handleSchedules(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	tasks := s.Schedules.Store().List()
	out := make([]Schedule, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, Schedule{Task: task, NextRun: task.NextRun(now)})
	}
	WriteJSON(w, http.StatusOK, ScheduleList{Schedules: out, Timezone: schedule.LocalZone(now)})
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	var task schedule.Task
	if !Decode(w, r, &task) {
		return
	}
	if err := s.validateScheduleTarget(task); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.Schedules.Store().Create(task)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	WriteJSON(w, http.StatusCreated, Schedule{Task: created, NextRun: created.NextRun(time.Now())})
}

func (s *Server) handleUpdateSchedule(w http.ResponseWriter, r *http.Request) {
	var task schedule.Task
	if !Decode(w, r, &task) {
		return
	}
	if err := s.validateScheduleTarget(task); err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	updated, err := s.Schedules.Store().Update(r.PathValue("id"), task)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, Schedule{Task: updated, NextRun: updated.NextRun(time.Now())})
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if err := s.Schedules.Store().Delete(r.PathValue("id")); err != nil {
		WriteError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.Schedules.Store().Get(r.PathValue("id")); !ok {
		WriteError(w, http.StatusNotFound, "schedule not found")
		return
	}
	id, err := s.Schedules.RunNow(r.PathValue("id"))
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.WriteCreated(w, r, id)
}
