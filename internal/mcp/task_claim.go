package mcp

import (
	"context"
	"errors"
	"time"

	"github.com/jdanielnd/crm-cli/internal/model"
	gomcp "github.com/mark3labs/mcp-go/mcp"
)

// Task claims (fork): the CLI's next/claim/note/wait/release plus the done
// the entity tools lack, over the same repository.

const claimRule = " Pick work with crm_task_next, or crm_task_claim a task from crm_task_list. " +
	"Never act on a task you do not hold. Call crm_task_note before each external side effect (send, draft, message)."

type taskNextArgs struct {
	As string `json:"as" jsonschema_description:"Your locator: <runtime>:<where>:<session>, unique per live agent session (e.g. openbot:Pessoal:<thread>)"`
}

type taskClaimArgs struct {
	ID       int64  `json:"id" jsonschema_description:"Task ID"`
	As       string `json:"as" jsonschema_description:"Your locator: <runtime>:<where>:<session>"`
	TakeOver bool   `json:"take_over,omitempty" jsonschema_description:"Replace the holder of a hold idle 2h or more; check the holder at its locator first"`
}

type taskNoteArgs struct {
	ID   int64  `json:"id" jsonschema_description:"Task ID"`
	As   string `json:"as" jsonschema_description:"Your locator (you must hold the task)"`
	Note string `json:"note" jsonschema_description:"Progress note: what you are about to do"`
}

type taskWaitArgs struct {
	ID   int64   `json:"id" jsonschema_description:"Task ID"`
	As   string  `json:"as" jsonschema_description:"Your locator (you must hold the task)"`
	Link string  `json:"link" jsonschema_description:"Artifact the owner must act on, e.g. a Gmail draft URL"`
	Note *string `json:"note,omitempty" jsonschema_description:"Progress note"`
}

type taskReleaseArgs struct {
	ID   int64   `json:"id" jsonschema_description:"Task ID"`
	As   *string `json:"as,omitempty" jsonschema_description:"Your locator; required to release a held task, not a waiting one"`
	Note *string `json:"note,omitempty" jsonschema_description:"Note for the next agent (e.g. why a draft was rejected)"`
}

type taskDoneArgs struct {
	ID int64 `json:"id" jsonschema_description:"Task ID"`
}

// taskNextResult is never an error result, so the agent sees stale holds
// even when the queue is empty (task null).
type taskNextResult struct {
	Task       *model.TaskWithClaim `json:"task"`
	StaleHolds []model.StaleHold    `json:"stale_holds"`
}

func (s *Server) registerTaskClaimTools() {
	s.mcp.AddTool(gomcp.NewTool("crm_task_next",
		gomcp.WithDescription("Hold the most urgent free task due today or earlier (or undated), in one atomic step. "+
			"Calling it again with the same as returns the task you already hold. task is null when nothing is free; "+
			"stale_holds lists holds idle 2h or more."+claimRule),
		gomcp.WithInputSchema[taskNextArgs]()), s.taskNext)
	s.mcp.AddTool(gomcp.NewTool("crm_task_claim",
		gomcp.WithDescription("Hold a chosen task (future-dated ones too). take_over replaces a holder idle 2h or more."+claimRule),
		gomcp.WithInputSchema[taskClaimArgs]()), s.taskClaim)
	s.mcp.AddTool(gomcp.NewTool("crm_task_note",
		gomcp.WithDescription("Record progress on a task you hold and reset its idle time. A conflict means you lost the hold: stop."+claimRule),
		gomcp.WithInputSchema[taskNoteArgs]()), s.taskNote)
	s.mcp.AddTool(gomcp.NewTool("crm_task_wait",
		gomcp.WithDescription("Hand a task you hold to the owner: it becomes waiting, with the artifact link, and no agent picks it. "+
			"You are then free to hold another task."+claimRule),
		gomcp.WithInputSchema[taskWaitArgs]()), s.taskWait)
	s.mcp.AddTool(gomcp.NewTool("crm_task_release",
		gomcp.WithDescription("Free a task: a held one by its holder (as), a waiting one by anyone. The link is cleared, the note kept."+claimRule),
		gomcp.WithInputSchema[taskReleaseArgs]()), s.taskRelease)
	s.mcp.AddTool(gomcp.NewTool("crm_task_done",
		gomcp.WithDescription("Mark a task completed; anyone may, and its claim is removed."+claimRule),
		gomcp.WithInputSchema[taskDoneArgs]()), s.taskDone)
}

func claimResult(t *model.Task, c *model.Claim, err error) (*gomcp.CallToolResult, error) {
	if err != nil {
		return mcpError(err)
	}
	return jsonResult(model.TaskWithClaim{Task: t, Claim: c})
}

func (s *Server) taskNext(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a taskNextArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	now := time.Now()
	t, c, err := s.claims.Next(ctx, a.As, now, now.Format("2006-01-02"))
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return mcpError(err)
	}
	stale, serr := s.claims.Stale(ctx, now)
	if serr != nil {
		return mcpError(serr)
	}
	res := taskNextResult{StaleHolds: stale}
	if err == nil {
		res.Task = &model.TaskWithClaim{Task: t, Claim: c}
	}
	return jsonResult(res)
}

func (s *Server) taskClaim(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a taskClaimArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	if a.TakeOver {
		return claimResult(s.claims.TakeOver(ctx, a.ID, a.As, time.Now()))
	}
	return claimResult(s.claims.Claim(ctx, a.ID, a.As, time.Now()))
}

func (s *Server) taskNote(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a taskNoteArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	return claimResult(s.claims.Note(ctx, a.ID, a.As, a.Note, time.Now()))
}

func (s *Server) taskWait(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a taskWaitArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	return claimResult(s.claims.Wait(ctx, a.ID, a.As, a.Link, a.Note, time.Now()))
}

func (s *Server) taskRelease(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a taskReleaseArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	return claimResult(s.claims.Release(ctx, a.ID, a.As, a.Note, time.Now()))
}

func (s *Server) taskDone(ctx context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
	var a taskDoneArgs
	if r := bind(req, &a); r != nil {
		return r, nil
	}
	t, err := s.tasks.Complete(ctx, a.ID)
	if err != nil {
		return mcpError(err)
	}
	return jsonResult(model.TaskWithClaim{Task: t})
}
