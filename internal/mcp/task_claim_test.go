package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callList invokes a tool whose result is a JSON array (call decodes
// objects only).
func callList(t *testing.T, c *client.Client, name string, args map[string]any) []map[string]any {
	t.Helper()
	req := gomcp.CallToolRequest{}
	req.Params.Name, req.Params.Arguments = name, args
	res, err := c.CallTool(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	tc, isText := res.Content[0].(gomcp.TextContent)
	require.True(t, isText)
	require.False(t, res.IsError, tc.Text)
	var out []map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &out), tc.Text)
	return out
}

func TestTaskClaimTools(t *testing.T) {
	for _, kind := range transports {
		t.Run(kind, func(t *testing.T) {
			c := connect(t, newBatchServer(t), kind)

			out := ok(t, c, "crm_task_next", map[string]any{"as": "test:mcp:a"})
			assert.Nil(t, out["task"], "empty queue is not an error")
			assert.Equal(t, []any{}, out["stale_holds"])

			for _, title := range []string{"first", "second"} {
				ok(t, c, "crm_task_create", map[string]any{"title": title})
			}

			a := ok(t, c, "crm_task_next", map[string]any{"as": "test:mcp:a"})["task"].(map[string]any)
			b := ok(t, c, "crm_task_next", map[string]any{"as": "test:mcp:b"})["task"].(map[string]any)
			assert.NotEqual(t, a["id"], b["id"], "two locators get different tasks")
			assert.Equal(t, "held", a["claim_state"])
			assert.Equal(t, "test:mcp:a", a["claim_holder"])
			assert.Equal(t, "first", a["title"], "the task keys are flattened next to claim_*")

			_, isErr, text := call(t, c, "crm_task_claim", map[string]any{"id": a["id"], "as": "test:mcp:c"})
			assert.True(t, isErr)
			assert.Contains(t, text, "test:mcp:a", "the conflict names the holder")

			_, isErr, text = call(t, c, "crm_task_next", map[string]any{"as": "bad"})
			assert.True(t, isErr)
			assert.Contains(t, text, "validation error")

			ok(t, c, "crm_task_note", map[string]any{"id": a["id"], "as": "test:mcp:a", "note": "creating draft"})
			w := ok(t, c, "crm_task_wait", map[string]any{"id": a["id"], "as": "test:mcp:a", "link": "https://mail/draft/1"})
			assert.Equal(t, "waiting", w["claim_state"])
			assert.Equal(t, "creating draft", w["claim_note"])

			waiting := callList(t, c, "crm_task_list", map[string]any{"waiting": true})
			require.Len(t, waiting, 1)
			assert.Equal(t, a["id"], waiting[0]["id"])
			assert.Equal(t, "https://mail/draft/1", waiting[0]["claim_link"])
			all := callList(t, c, "crm_task_list", map[string]any{})
			assert.Len(t, all, 2)

			done := ok(t, c, "crm_task_done", map[string]any{"id": a["id"]})
			assert.Equal(t, true, done["completed"])
			assert.Nil(t, done["claim_state"])
			assert.Empty(t, callList(t, c, "crm_task_list", map[string]any{"waiting": true}), "the trigger cleared the claim")

			r := ok(t, c, "crm_task_release", map[string]any{"id": b["id"], "as": "test:mcp:b", "note": "later"})
			assert.Equal(t, "free", r["claim_state"])
			assert.Equal(t, "test:mcp:b", r["claim_previous_holder"])
		})
	}
}
