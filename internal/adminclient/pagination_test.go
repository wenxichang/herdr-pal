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

func TestListConnectionsAggregatesPagesOnOneConnection(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	var dials atomic.Int32
	client := newPipeAdminClient(t, func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		return clientSide, nil
	})
	firstObserved := time.Date(2026, 7, 28, 16, 0, 0, 0, time.UTC)
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
			if request.Method != adminproto.MethodConnectionList {
				serverDone <- errors.New("unexpected method")
				return
			}
			var params adminproto.ConnectionListParams
			if err := adminproto.DecodeParams(request.Params, &params); err != nil {
				serverDone <- err
				return
			}
			if page == 1 && params.PageToken != "" || page == 2 && params.PageToken != "connection-next" {
				serverDone <- errors.New("unexpected connection page token")
				return
			}
			result := adminproto.ConnectionListResult{
				ObservedAt: firstObserved,
				Items:      []adminproto.Connection{{ConnectionID: "connection-" + string(rune('0'+page))}},
			}
			if page == 1 {
				result.NextPageToken = "connection-next"
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
	result, err := client.ListConnections(t.Context(), adminproto.ConnectionListParams{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 || len(result.Items) != 2 || result.Items[0].ConnectionID != "connection-1" || result.Items[1].ConnectionID != "connection-2" || result.ObservedAt != secondObserved || result.NextPageToken != "" {
		t.Fatalf("aggregated result=%#v dials=%d", result, dials.Load())
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestListSessionsAggregatesPagesOnOneConnection(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	defer serverSide.Close()
	var dials atomic.Int32
	client := newPipeAdminClient(t, func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		return clientSide, nil
	})
	firstObserved := time.Date(2026, 7, 28, 17, 0, 0, 0, time.UTC)
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
			if request.Method != adminproto.MethodSessionList {
				serverDone <- errors.New("unexpected method")
				return
			}
			var params adminproto.SessionListParams
			if err := adminproto.DecodeParams(request.Params, &params); err != nil {
				serverDone <- err
				return
			}
			if params.PrincipalID != "user-a" || params.MachineID != "home" || page == 1 && params.PageToken != "" || page == 2 && params.PageToken != "session-next" {
				serverDone <- errors.New("unexpected session filters or page token")
				return
			}
			result := adminproto.SessionListResult{
				ObservedAt: firstObserved,
				Items: []adminproto.Session{{
					PrincipalID: "user-a", Number: page,
					Target: adminproto.SessionTarget{MachineID: "home", SlotID: "pane-" + string(rune('0'+page)), SessionID: "session-" + string(rune('0'+page))},
				}},
			}
			if page == 1 {
				result.NextPageToken = "session-next"
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
	result, err := client.ListSessions(t.Context(), adminproto.SessionListParams{PrincipalID: "user-a", MachineID: "home", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 1 || len(result.Items) != 2 || result.Items[0].Target.SessionID != "session-1" || result.Items[1].Target.SessionID != "session-2" || result.ObservedAt != secondObserved || result.NextPageToken != "" {
		t.Fatalf("aggregated result=%#v dials=%d", result, dials.Load())
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestAutomaticPaginationRejectsRepeatedPageToken(t *testing.T) {
	tests := []struct {
		name   string
		result any
		call   func(*Client) error
	}{
		{name: "key", result: adminproto.KeyListResult{NextPageToken: "repeat"}, call: func(client *Client) error {
			_, err := client.ListKeys(t.Context(), adminproto.KeyListParams{})
			return err
		}},
		{name: "connection", result: adminproto.ConnectionListResult{NextPageToken: "repeat"}, call: func(client *Client) error {
			_, err := client.ListConnections(t.Context(), adminproto.ConnectionListParams{})
			return err
		}},
		{name: "session", result: adminproto.SessionListResult{NextPageToken: "repeat"}, call: func(client *Client) error {
			_, err := client.ListSessions(t.Context(), adminproto.SessionListParams{})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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
					response, _ := adminproto.NewResultResponse(request.ID, test.result)
					encoded, _ := adminproto.EncodeResponse(response)
					_, _ = serverSide.Write(encoded)
				}
			}()
			if err := test.call(client); !errors.Is(err, ErrProtocol) {
				t.Fatalf("automatic pagination error = %v, want ErrProtocol", err)
			}
		})
	}
}
