// Code generated by falafel 0.9.1. DO NOT EDIT.
// source: client.proto

//go:build js
// +build js

package looprpc

import (
	"context"

	gateway "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

func RegisterSwapClientJSONCallbacks(registry map[string]func(ctx context.Context,
	conn *grpc.ClientConn, reqJSON string, callback func(string, error))) {

	marshaler := &gateway.JSONPb{
		MarshalOptions: protojson.MarshalOptions{
			UseProtoNames:   true,
			EmitUnpopulated: true,
		},
	}

	registry["looprpc.SwapClient.LoopOut"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &LoopOutRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.LoopOut(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.LoopIn"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &LoopInRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.LoopIn(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.Monitor"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &MonitorRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		stream, err := client.Monitor(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		go func() {
			for {
				select {
				case <-stream.Context().Done():
					callback("", stream.Context().Err())
					return
				default:
				}

				resp, err := stream.Recv()
				if err != nil {
					callback("", err)
					return
				}

				respBytes, err := marshaler.Marshal(resp)
				if err != nil {
					callback("", err)
					return
				}
				callback(string(respBytes), nil)
			}
		}()
	}

	registry["looprpc.SwapClient.ListSwaps"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &ListSwapsRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.ListSwaps(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.SwapInfo"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &SwapInfoRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.SwapInfo(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.LoopOutTerms"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &TermsRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.LoopOutTerms(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.LoopOutQuote"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &QuoteRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.LoopOutQuote(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.GetLoopInTerms"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &TermsRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.GetLoopInTerms(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.GetLoopInQuote"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &QuoteRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.GetLoopInQuote(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.Probe"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &ProbeRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.Probe(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.GetLsatTokens"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &TokensRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.GetLsatTokens(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.GetLiquidityParams"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &GetLiquidityParamsRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.GetLiquidityParams(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.SetLiquidityParams"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &SetLiquidityParamsRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.SetLiquidityParams(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}

	registry["looprpc.SwapClient.SuggestSwaps"] = func(ctx context.Context,
		conn *grpc.ClientConn, reqJSON string, callback func(string, error)) {

		req := &SuggestSwapsRequest{}
		err := marshaler.Unmarshal([]byte(reqJSON), req)
		if err != nil {
			callback("", err)
			return
		}

		client := NewSwapClientClient(conn)
		resp, err := client.SuggestSwaps(ctx, req)
		if err != nil {
			callback("", err)
			return
		}

		respBytes, err := marshaler.Marshal(resp)
		if err != nil {
			callback("", err)
			return
		}
		callback(string(respBytes), nil)
	}
}
