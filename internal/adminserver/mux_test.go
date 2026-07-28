package adminserver

import (
	"context"
	"testing"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

func TestServerMethodMuxRoutesAndRejectsDuplicateMethods(t *testing.T) {
	first := methodHandlerStub{methods: []adminproto.Method{adminproto.MethodServerStatus}, value: "first"}
	second := methodHandlerStub{methods: []adminproto.Method{adminproto.MethodKeyList}, value: "second"}
	mux, err := NewMethodMux(first, second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := mux.Handle(t.Context(), adminproto.Request{Method: adminproto.MethodKeyList})
	if err != nil || string(result.Response.Result) != `"second"` {
		t.Fatalf("mux result=%#v error=%v", result, err)
	}
	if _, err := NewMethodMux(first, first); err == nil {
		t.Fatal("duplicate method registration was accepted")
	}
}

type methodHandlerStub struct {
	methods []adminproto.Method
	value   string
}

func (handler methodHandlerStub) Methods() []adminproto.Method {
	return append([]adminproto.Method(nil), handler.methods...)
}

func (handler methodHandlerStub) Handle(context.Context, adminproto.Request) (HandleResult, error) {
	response, err := adminproto.NewResultResponse("req-1", handler.value)
	return HandleResult{Response: response}, err
}
