//
// Tencent is pleased to support the open source community by making trpc-agent-go available.
//
// Copyright (C) 2025 Tencent.  All rights reserved.
//
// trpc-agent-go is licensed under the Apache License Version 2.0.
//

package toolcode

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"trpc.group/trpc-go/trpc-agent-go/agent"
	bridge "trpc.group/trpc-go/trpc-agent-go/codeexecutor/codeact"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

type addTool struct{}

func (addTool) Declaration() *tool.Declaration {
	return &tool.Declaration{Name: "add", InputSchema: &tool.Schema{Type: "object", Required: []string{"a", "b"}, Properties: map[string]*tool.Schema{"a": {Type: "integer"}, "b": {Type: "integer"}}}}
}
func (addTool) Call(_ context.Context, raw []byte) (any, error) {
	var v struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v.A + v.B, nil
}

func TestToolRunsGuestThroughGateway(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	toolValue, err := NewTool(bridge.LocalRunner{}, []tool.CallableTool{addTool{}})
	require.NoError(t, err)
	require.Equal(t, "execute_tool_code", toolValue.Declaration().Name)
	require.Contains(t, toolValue.Declaration().Description, "add")
	require.Contains(t, toolValue.Declaration().Description, "Prefer one execute_tool_code call")
	require.Contains(t, toolValue.Declaration().Description, "Calls execute sequentially")
	require.Contains(t, toolValue.Declaration().Description, "compact JSON-compatible value")
	require.Contains(t, toolValue.Declaration().Description, "Python RuntimeError")
	require.Contains(t, toolValue.Declaration().Description, "direct HTTP clients")
	require.Contains(t, toolValue.Declaration().Description, "Host capabilities available inside Python")
	require.Contains(t, toolValue.Declaration().Description, "Input JSON Schema")
	require.Contains(t, toolValue.Declaration().Description, "\"a\"")
	result, err := toolValue.Call(context.Background(), []byte(`{"code":"return await call_tool('add', a=1, b=41)"}`))
	require.NoError(t, err)
	got := result.(bridge.Result)
	require.JSONEq(t, "42", string(got.Value))
}

func TestNewToolOptionsAndValidation(t *testing.T) {
	toolValue, err := NewTool(nil, nil, WithName("orchestrate"), WithDescription("custom description"))
	require.NoError(t, err)
	decl := toolValue.Declaration()
	require.Equal(t, "orchestrate", decl.Name)
	require.Equal(t, "custom description", decl.Description)

	_, err = NewTool(nil, nil, WithName(" \t"))
	require.ErrorContains(t, err, "tool name is required")
	_, err = NewTool(nil, []tool.CallableTool{nil})
	require.ErrorContains(t, err, "tool declaration is required")
}

func TestToolCallValidationAndRuntime(t *testing.T) {
	runtime := &recordingRuntime{result: bridge.Result{Value: json.RawMessage(`{"ok":true}`)}}
	toolValue, err := NewTool(runtime, nil)
	require.NoError(t, err)

	_, err = toolValue.Call(context.Background(), []byte(`not-json`))
	require.Error(t, err)
	_, err = toolValue.Call(context.Background(), []byte(`{"code":""}`))
	require.ErrorContains(t, err, "code is required")

	result, err := toolValue.Call(context.Background(), []byte(`{"code":"return {'ok': True}"}`))
	require.NoError(t, err)
	require.Equal(t, "return {'ok': True}", runtime.request.Code)
	require.Equal(t, "python", runtime.request.Language)
	require.Equal(t, runtime.result, result)

	nilRuntimeTool, err := NewTool(nil, nil)
	require.NoError(t, err)
	_, err = nilRuntimeTool.Call(context.Background(), []byte(`{"code":"return 1"}`))
	require.ErrorContains(t, err, "runtime is required")
}

func TestBuildToolHelpSortsSchemasAndSkipsInvalidDeclarations(t *testing.T) {
	help := buildToolHelp([]tool.CallableTool{
		nil,
		declaredTool{},
		declaredTool{decl: &tool.Declaration{Name: "zeta", Description: "zeta description", InputSchema: &tool.Schema{Type: "object"}}},
		declaredTool{decl: &tool.Declaration{Name: "alpha", Description: "alpha description", OutputSchema: &tool.Schema{Type: "string"}}},
	})
	require.Less(t, strings.Index(help, "alpha"), strings.Index(help, "zeta"))
	require.Contains(t, help, "Input JSON Schema")
	require.Contains(t, help, "Output JSON Schema")
}

type recordingRuntime struct {
	request bridge.Request
	result  bridge.Result
}

func (r *recordingRuntime) ExecuteCodeAct(_ context.Context, req bridge.Request, _ bridge.ToolCallHandler) (bridge.Result, error) {
	r.request = req
	return r.result, nil
}

type declaredTool struct{ decl *tool.Declaration }

func (t declaredTool) Declaration() *tool.Declaration { return t.decl }
func (declaredTool) Call(context.Context, []byte) (any, error) {
	return nil, nil
}

type countingDocumentTool struct {
	calls int
}

func (t *countingDocumentTool) Declaration() *tool.Declaration {
	return &tool.Declaration{
		Name: "read_document",
		InputSchema: &tool.Schema{
			Type:       "object",
			Required:   []string{"id"},
			Properties: map[string]*tool.Schema{"id": {Type: "string"}},
		},
	}
}

func (t *countingDocumentTool) Call(context.Context, []byte) (any, error) {
	t.calls++
	return map[string]string{"content": "secret"}, nil
}

type toolCallingRuntime struct {
	call bridge.ToolCall
}

func (r *toolCallingRuntime) ExecuteCodeAct(ctx context.Context, _ bridge.Request, handler bridge.ToolCallHandler) (bridge.Result, error) {
	raw, err := handler.HandleToolCall(ctx, r.call)
	if err != nil {
		return bridge.Result{}, err
	}
	return bridge.Result{Value: raw}, nil
}

func TestToolParentRunToolPermissionPolicyOptIn(t *testing.T) {
	readDocument := &countingDocumentTool{}
	runtime := &toolCallingRuntime{call: bridge.ToolCall{
		ID:   "inner-1",
		Name: "read_document",
		Args: json.RawMessage(`{"id":"doc-1"}`),
	}}
	var got *tool.PermissionRequest
	policy := tool.PermissionPolicyFunc(func(_ context.Context, req *tool.PermissionRequest) (tool.PermissionDecision, error) {
		got = req
		return tool.DenyPermission("document is off limits"), nil
	})
	invocation := &agent.Invocation{RunOptions: agent.NewRunOptions(agent.WithToolPermissionPolicy(policy))}
	ctx := agent.NewInvocationContext(context.Background(), invocation)

	checked, err := NewTool(runtime, []tool.CallableTool{readDocument}, WithParentRunToolPermissionPolicy())
	require.NoError(t, err)
	result, err := checked.Call(ctx, []byte(`{"code":"return await call_tool('read_document', id='doc-1')"}`))
	require.NoError(t, err)
	denied := result.(bridge.Result)
	require.JSONEq(t, `{"status":"denied","tool":"read_document","reason":"document is off limits"}`, string(denied.Value))
	require.Zero(t, readDocument.calls)
	require.NotNil(t, got)
	require.Equal(t, "inner-1", got.ToolCallID)
	require.Equal(t, "read_document", got.ToolName)
	require.JSONEq(t, `{"id":"doc-1"}`, string(got.Arguments))
	require.NotNil(t, got.Declaration)

	unchecked, err := NewTool(runtime, []tool.CallableTool{readDocument})
	require.NoError(t, err)
	result, err = unchecked.Call(ctx, []byte(`{"code":"return await call_tool('read_document', id='doc-1')"}`))
	require.NoError(t, err)
	allowed := result.(bridge.Result)
	require.JSONEq(t, `{"content":"secret"}`, string(allowed.Value))
	require.Equal(t, 1, readDocument.calls)
}

func TestToolParentRunToolPermissionPolicyDefaultsToAllow(t *testing.T) {
	readDocument := &countingDocumentTool{}
	runtime := &toolCallingRuntime{call: bridge.ToolCall{
		ID:   "inner-1",
		Name: "read_document",
		Args: json.RawMessage(`{"id":"doc-1"}`),
	}}
	toolValue, err := NewTool(runtime, []tool.CallableTool{readDocument}, WithParentRunToolPermissionPolicy())
	require.NoError(t, err)

	result, err := toolValue.Call(context.Background(), []byte(`{"code":"return await call_tool('read_document', id='doc-1')"}`))
	require.NoError(t, err)
	got := result.(bridge.Result)
	require.JSONEq(t, `{"content":"secret"}`, string(got.Value))
	require.Equal(t, 1, readDocument.calls)

	ctx := agent.NewInvocationContext(context.Background(), &agent.Invocation{})
	result, err = toolValue.Call(ctx, []byte(`{"code":"return await call_tool('read_document', id='doc-1')"}`))
	require.NoError(t, err)
	got = result.(bridge.Result)
	require.JSONEq(t, `{"content":"secret"}`, string(got.Value))
	require.Equal(t, 2, readDocument.calls)
}

func TestToolParentRunToolPermissionPolicyError(t *testing.T) {
	readDocument := &countingDocumentTool{}
	runtime := &toolCallingRuntime{call: bridge.ToolCall{
		ID:   "inner-1",
		Name: "read_document",
		Args: json.RawMessage(`{"id":"doc-1"}`),
	}}
	policy := tool.PermissionPolicyFunc(func(context.Context, *tool.PermissionRequest) (tool.PermissionDecision, error) {
		return tool.PermissionDecision{}, errors.New("policy unavailable")
	})
	ctx := agent.NewInvocationContext(context.Background(), &agent.Invocation{
		RunOptions: agent.NewRunOptions(agent.WithToolPermissionPolicy(policy)),
	})
	toolValue, err := NewTool(runtime, []tool.CallableTool{readDocument}, WithParentRunToolPermissionPolicy())
	require.NoError(t, err)

	_, err = toolValue.Call(ctx, []byte(`{"code":"return await call_tool('read_document', id='doc-1')"}`))
	require.ErrorContains(t, err, "policy unavailable")
	require.Zero(t, readDocument.calls)
}
