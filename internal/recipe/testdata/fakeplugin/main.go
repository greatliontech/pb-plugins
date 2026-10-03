// fakeplugin answers the plugin protocol as PROBE_MODE says, for
// recipe.TestProbe: file (a response holding one file named for the
// request's file), error (the plugin's own error), features (an empty
// response carrying supported_features), empty (nothing written),
// garbage (text no response parses from), exit3 (a message on
// standard error and exit status 3).
package main

import (
	"fmt"
	"io"
	"os"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var req pluginpb.CodeGeneratorRequest
	if err := proto.Unmarshal(in, &req); err != nil || len(req.FileToGenerate) != 1 || len(req.ProtoFile) != 1 {
		fmt.Fprintf(os.Stderr, "fakeplugin: no request over one file: %v %v\n", err, req.FileToGenerate)
		os.Exit(2)
	}
	var resp *pluginpb.CodeGeneratorResponse
	switch os.Getenv("PROBE_MODE") {
	case "file":
		resp = &pluginpb.CodeGeneratorResponse{File: []*pluginpb.CodeGeneratorResponse_File{{Name: proto.String(req.FileToGenerate[0] + ".txt"), Content: proto.String("x")}}}
	case "error":
		resp = &pluginpb.CodeGeneratorResponse{Error: proto.String("go_package missing")}
	case "features":
		resp = &pluginpb.CodeGeneratorResponse{SupportedFeatures: proto.Uint64(1)}
	case "empty":
		return
	case "garbage":
		fmt.Print("hello world")
		return
	case "parameter":
		// A plugin that answers nothing without its parameter.
		if req.GetParameter() != "p=1" {
			fmt.Fprintln(os.Stderr, "parameter p=1 required")
			os.Exit(1)
		}
		resp = &pluginpb.CodeGeneratorResponse{File: []*pluginpb.CodeGeneratorResponse_File{{Name: proto.String(req.FileToGenerate[0] + ".txt"), Content: proto.String("x")}}}
	case "exit3":
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	default:
		fmt.Fprintln(os.Stderr, "fakeplugin: PROBE_MODE unset")
		os.Exit(2)
	}
	out, err := proto.Marshal(resp)
	if err != nil {
		os.Exit(2)
	}
	os.Stdout.Write(out)
}
