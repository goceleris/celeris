// Package protobuf provides Protocol Buffers serialization for celeris.
//
// Protocol Buffers was removed from the celeris core to avoid forcing
// google.golang.org/protobuf as a transitive dependency. This package
// provides equivalent functionality as standalone functions.
//
// # Basic Usage
//
// Write a protobuf response:
//
//	protobuf.Write(c, 200, &myProto)
//
// Parse a protobuf request:
//
//	var msg pb.MyMessage
//	if err := protobuf.BindProtoBuf(c, &msg); err != nil {
//	    return err
//	}
//
// Auto-detect content type:
//
//	var msg pb.MyMessage
//	if err := protobuf.Bind(c, &msg); err != nil {
//	    return err
//	}
//
// # Content Negotiation
//
// Use [Respond] to serve protobuf or JSON based on the Accept header:
//
//	protobuf.Respond(c, 200, &myProto, myJSONStruct)
//
// # Middleware with Custom Options
//
// Install the middleware for custom marshal/unmarshal options:
//
//	s.Use(protobuf.New(protobuf.Config{
//	    MarshalOptions: proto.MarshalOptions{Deterministic: true},
//	}))
//
// Then use [FromContext] in handlers:
//
//	pb := protobuf.FromContext(c)
//	pb.Write(200, &myProto)
//
// # Content Types
//
// Both "application/x-protobuf" (primary) and "application/protobuf" are
// recognized. Responses use "application/x-protobuf".
package protobuf
