package adminclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wenxichang/herdr-pal/internal/adminproto"
)

func TestListKeysAggregatesPagesOnOneConnection(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	var dials atomic.Int32
	client := newPipeAdminClient(t, func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		return clientSide, nil
	})
	firstObserved := time.Date(2026, 7, 28, 15, 0, 0, 0, time.UTC)
	secondObserved := firstObserved.Add(time.Second)
	serverDone := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(serverSide)
		for page := 1; page <= 2; page++ {
			frame, err := adminproto.ReadFrame(reader)
			if err != nil {
				serverDone <- err
				return
			}
			request, err := adminproto.DecodeRequest(frame)
			if err != nil {
				serverDone <- err
				return
			}
			var params adminproto.KeyListParams
			if err := adminproto.DecodeParams(request.Params, &params); err != nil {
				serverDone <- err
				return
			}
			if page == 1 && params.PageToken != "" || page == 2 && params.PageToken != "next-1" {
				serverDone <- errors.New("unexpected page token")
				return
			}
			result := adminproto.KeyListResult{ObservedAt: firstObserved, Items: []adminproto.Credential{{CredentialID: uint64(page)}}}
			if page == 1 {
				result.NextPageToken = "next-1"
			} else {
				result.ObservedAt = secondObserved
			}
			response, _ := adminproto.NewResultResponse(request.ID, result)
			encoded, _ := adminproto.EncodeResponse(response)
			if _, err := serverSide.Write(encoded); err != nil {
				serverDone <- err
				return
			}
		}
		serverDone <- nil
	}()
	result, err := client.ListKeys(t.Context(), adminproto.KeyListParams{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 || len(result.Items) != 2 || result.Items[0].CredentialID != 1 || result.Items[1].CredentialID != 2 || result.ObservedAt != secondObserved || result.NextPageToken != "" {
		t.Fatalf("aggregated result=%#v dials=%d", result, dials.Load())
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticPaginationRejectsRepeatedPageToken(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	client := newPipeAdminClient(t, func(context.Context, string) (net.Conn, error) { return clientSide, nil })
	go func() {
		reader := bufio.NewReader(serverSide)
		for range 2 {
			frame, err := adminproto.ReadFrame(reader)
			if err != nil {
				return
			}
			request, _ := adminproto.DecodeRequest(frame)
			response, _ := adminproto.NewResultResponse(request.ID, adminproto.KeyListResult{NextPageToken: "repeat"})
			encoded, _ := adminproto.EncodeResponse(response)
			_, _ = serverSide.Write(encoded)
		}
	}()
	_, err := client.ListKeys(t.Context(), adminproto.KeyListParams{})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("ListKeys() error = %v, want ErrProtocol", err)
	}
}
