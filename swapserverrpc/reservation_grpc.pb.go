// Code generated by protoc-gen-go-grpc. DO NOT EDIT.

package swapserverrpc

import (
	context "context"
	grpc "google.golang.org/grpc"
	codes "google.golang.org/grpc/codes"
	status "google.golang.org/grpc/status"
)

// This is a compile-time assertion to ensure that this generated file
// is compatible with the grpc package it is being compiled against.
// Requires gRPC-Go v1.32.0 or later.
const _ = grpc.SupportPackageIsVersion7

// ReservationServiceClient is the client API for ReservationService service.
//
// For semantics around ctx use and closing/ending streaming RPCs, please refer to https://pkg.go.dev/google.golang.org/grpc/?tab=doc#ClientConn.NewStream.
type ReservationServiceClient interface {
	// ReservationNotificationStream is a server side stream that sends
	// notifications if the server wants to open a reservation to the client.
	ReservationNotificationStream(ctx context.Context, in *ReservationNotificationRequest, opts ...grpc.CallOption) (ReservationService_ReservationNotificationStreamClient, error)
	// OpenReservation requests a new reservation UTXO from the server.
	OpenReservation(ctx context.Context, in *ServerOpenReservationRequest, opts ...grpc.CallOption) (*ServerOpenReservationResponse, error)
}

type reservationServiceClient struct {
	cc grpc.ClientConnInterface
}

func NewReservationServiceClient(cc grpc.ClientConnInterface) ReservationServiceClient {
	return &reservationServiceClient{cc}
}

func (c *reservationServiceClient) ReservationNotificationStream(ctx context.Context, in *ReservationNotificationRequest, opts ...grpc.CallOption) (ReservationService_ReservationNotificationStreamClient, error) {
	stream, err := c.cc.NewStream(ctx, &ReservationService_ServiceDesc.Streams[0], "/looprpc.ReservationService/ReservationNotificationStream", opts...)
	if err != nil {
		return nil, err
	}
	x := &reservationServiceReservationNotificationStreamClient{stream}
	if err := x.ClientStream.SendMsg(in); err != nil {
		return nil, err
	}
	if err := x.ClientStream.CloseSend(); err != nil {
		return nil, err
	}
	return x, nil
}

type ReservationService_ReservationNotificationStreamClient interface {
	Recv() (*ServerReservationNotification, error)
	grpc.ClientStream
}

type reservationServiceReservationNotificationStreamClient struct {
	grpc.ClientStream
}

func (x *reservationServiceReservationNotificationStreamClient) Recv() (*ServerReservationNotification, error) {
	m := new(ServerReservationNotification)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

func (c *reservationServiceClient) OpenReservation(ctx context.Context, in *ServerOpenReservationRequest, opts ...grpc.CallOption) (*ServerOpenReservationResponse, error) {
	out := new(ServerOpenReservationResponse)
	err := c.cc.Invoke(ctx, "/looprpc.ReservationService/OpenReservation", in, out, opts...)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReservationServiceServer is the server API for ReservationService service.
// All implementations must embed UnimplementedReservationServiceServer
// for forward compatibility
type ReservationServiceServer interface {
	// ReservationNotificationStream is a server side stream that sends
	// notifications if the server wants to open a reservation to the client.
	ReservationNotificationStream(*ReservationNotificationRequest, ReservationService_ReservationNotificationStreamServer) error
	// OpenReservation requests a new reservation UTXO from the server.
	OpenReservation(context.Context, *ServerOpenReservationRequest) (*ServerOpenReservationResponse, error)
	mustEmbedUnimplementedReservationServiceServer()
}

// UnimplementedReservationServiceServer must be embedded to have forward compatible implementations.
type UnimplementedReservationServiceServer struct {
}

func (UnimplementedReservationServiceServer) ReservationNotificationStream(*ReservationNotificationRequest, ReservationService_ReservationNotificationStreamServer) error {
	return status.Errorf(codes.Unimplemented, "method ReservationNotificationStream not implemented")
}
func (UnimplementedReservationServiceServer) OpenReservation(context.Context, *ServerOpenReservationRequest) (*ServerOpenReservationResponse, error) {
	return nil, status.Errorf(codes.Unimplemented, "method OpenReservation not implemented")
}
func (UnimplementedReservationServiceServer) mustEmbedUnimplementedReservationServiceServer() {}

// UnsafeReservationServiceServer may be embedded to opt out of forward compatibility for this service.
// Use of this interface is not recommended, as added methods to ReservationServiceServer will
// result in compilation errors.
type UnsafeReservationServiceServer interface {
	mustEmbedUnimplementedReservationServiceServer()
}

func RegisterReservationServiceServer(s grpc.ServiceRegistrar, srv ReservationServiceServer) {
	s.RegisterService(&ReservationService_ServiceDesc, srv)
}

func _ReservationService_ReservationNotificationStream_Handler(srv interface{}, stream grpc.ServerStream) error {
	m := new(ReservationNotificationRequest)
	if err := stream.RecvMsg(m); err != nil {
		return err
	}
	return srv.(ReservationServiceServer).ReservationNotificationStream(m, &reservationServiceReservationNotificationStreamServer{stream})
}

type ReservationService_ReservationNotificationStreamServer interface {
	Send(*ServerReservationNotification) error
	grpc.ServerStream
}

type reservationServiceReservationNotificationStreamServer struct {
	grpc.ServerStream
}

func (x *reservationServiceReservationNotificationStreamServer) Send(m *ServerReservationNotification) error {
	return x.ServerStream.SendMsg(m)
}

func _ReservationService_OpenReservation_Handler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(ServerOpenReservationRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(ReservationServiceServer).OpenReservation(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/looprpc.ReservationService/OpenReservation",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(ReservationServiceServer).OpenReservation(ctx, req.(*ServerOpenReservationRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// ReservationService_ServiceDesc is the grpc.ServiceDesc for ReservationService service.
// It's only intended for direct use with grpc.RegisterService,
// and not to be introspected or modified (even as a copy)
var ReservationService_ServiceDesc = grpc.ServiceDesc{
	ServiceName: "looprpc.ReservationService",
	HandlerType: (*ReservationServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "OpenReservation",
			Handler:    _ReservationService_OpenReservation_Handler,
		},
	},
	Streams: []grpc.StreamDesc{
		{
			StreamName:    "ReservationNotificationStream",
			Handler:       _ReservationService_ReservationNotificationStream_Handler,
			ServerStreams: true,
		},
	},
	Metadata: "reservation.proto",
}
