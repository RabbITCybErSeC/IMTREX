package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
)

// The manual CRUD endpoints of the overview's "goal management". They write the same goal nodes as the agent-side set_goals tool,
// but the entry point is a human adding/editing/deleting in the UI; an add/edit reuses the "revive the task" logic (admitTask resume:
// terminal -> running, clear the pause, queue when necessary), while a delete does not revive it (a product decision). Every change handler goes through
// beginTaskOperation/decInflight to avoid racing with task deletion (as with the intent CRUD).

// listGoals returns every goal of this task (with text/vulnclass/state split out), for the goal management card to render.
func (s *Server) listGoals(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	goals, err := t.Store.ListByKind(db.KindGoal, 10000)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"goals": goalDTOs(goals)})
}

// addGoal adds a goal by hand: persist it (attached under the task root's spawns) -> record a "a goal was added" trigger to wake
// the planner -> revive the task, so the planner re-judges achievement against the new goal.
func (s *Server) addGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "the task is being deleted, so a goal cannot be added")
		return
	}
	defer s.engine.decInflight(t.ID)

	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "the goal content must not be empty")
		return
	}
	payload := map[string]any{"text": text}
	if vc := strings.TrimSpace(body.VulnClass); vc != "" {
		payload["vulnclass"] = vc
	}
	id, err := t.Store.AddGoal(payload, "human")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if of, _ := t.Store.OriginFactID(); of > 0 && id > 0 {
		_ = t.Store.Link(of, db.RelSpawns, id) // goal descends from the task root (origin fact)
	}
	t.NotifyGoal([]string{text}) // record a "a human added a goal: ..." trigger and wake the planner
	s.reviveTask(t)              // pull a completed/paused task back into the running state
	node, _ := t.Store.GetNode(id)
	if node == nil {
		writeErr(w, 500, "the goal could not be read back after it was written")
		return
	}
	writeJSON(w, 200, goalDTO(node))
}

// editGoal edits a goal's text (and vulnclass) by hand: update the database -> record a "the user changed a goal from old to new"
// trigger to wake the planner -> revive the task, so the planner adjusts direction to the new goal.
func (s *Server) editGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "the task is being deleted, so a goal cannot be edited")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	var body struct {
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, 400, "the goal content must not be empty")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "the goal does not exist")
		return
	}
	oldText := goalDTO(node).Text
	if err := t.Store.UpdateGoalPayload(gid, text, strings.TrimSpace(body.VulnClass)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalEdited(oldText, text) // record a "a human changed a goal from old to new" trigger and wake the planner
	s.reviveTask(t)                   // as with adding: revive the task so it re-judges against the new goal
	updated, _ := t.Store.GetNode(gid)
	if updated == nil {
		writeErr(w, 500, "the goal could not be read back after it was updated")
		return
	}
	writeJSON(w, 200, goalDTO(updated))
}

// deleteGoal deletes a goal by hand (a hard delete that cascades to its edges/anchors): delete it -> record a "the user deleted goal X"
// trigger to wake the planner so it re-judges the remaining goals. By product decision, deleting does **not** revive the task.
func (s *Server) deleteGoal(w http.ResponseWriter, r *http.Request) {
	t, ok := s.m.Task(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "task not found")
		return
	}
	if !s.engine.beginTaskOperation(t.ID) {
		writeErr(w, 409, "the task is being deleted, so a goal cannot be deleted")
		return
	}
	defer s.engine.decInflight(t.ID)

	gid, err := strconv.ParseInt(r.PathValue("gid"), 10, 64)
	if err != nil || gid <= 0 {
		writeErr(w, 400, "bad goal id")
		return
	}
	node, err := t.Store.GetNode(gid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if node == nil || node.Kind != db.KindGoal {
		writeErr(w, 404, "the goal does not exist")
		return
	}
	text := goalDTO(node).Text
	if err := t.Store.DeleteGoal(gid); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	t.NotifyGoalDeleted(text) // record a "a human deleted this goal: ..." trigger and wake the planner (without reviving the task)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
