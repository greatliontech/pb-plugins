package recipe

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/greatliontech/pb-plugins/internal/catalog"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// probeTimeout bounds a plugin's answer to the probe.
const probeTimeout = 2 * time.Minute

// probeRequest is the plugin-protocol request the probe hands a built
// executable: one proto3 file of a package, a request and a response
// message, and a service with one rpc over them — the shapes every
// generator of the catalog acts on — with no parameter.
func probeRequest() ([]byte, error) {
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	field := func(name string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Type: &str, Label: &label, JsonName: proto.String(name)}
	}
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("probe/v1/probe.proto"),
		Package: proto.String("probe.v1"),
		Syntax:  proto.String("proto3"),
		Options: &descriptorpb.FileOptions{GoPackage: proto.String("example.com/probe/v1;probev1")},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("CallRequest"), Field: []*descriptorpb.FieldDescriptorProto{field("name", 1)}},
			{Name: proto.String("CallResponse"), Field: []*descriptorpb.FieldDescriptorProto{field("greeting", 1)}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name:   proto.String("ProbeService"),
			Method: []*descriptorpb.MethodDescriptorProto{{Name: proto.String("Call"), InputType: proto.String(".probe.v1.CallRequest"), OutputType: proto.String(".probe.v1.CallResponse")}},
		}},
	}
	return proto.Marshal(&pluginpb.CodeGeneratorRequest{FileToGenerate: []string{file.GetName()}, ProtoFile: []*descriptorpb.FileDescriptorProto{file}})
}

// probeTree probes the host's tree where one was built: the probe
// runs on the platform the build ran on, the one whose executable
// the host can start, and nowhere else.
func probeTree(ctx context.Context, p *catalog.Plugin, platforms []string, out string) error {
	for _, pl := range platforms {
		if pl == Host() {
			return Probe(ctx, filepath.Join(TreeDir(out, pl), p.Entrypoint), p.Silent)
		}
	}
	return nil
}

// Probe runs a built executable as a plugin over probeRequest and
// reads its answer: a plugin answers with a response holding a file;
// one that writes nothing, or bytes no response parses from, or
// exits non-zero, or answers with an error of its own — the plain
// file asks nothing a generator refuses — is refused, so a built
// tree that does not speak the plugin protocol is never published.
// A silent plugin, one generating only for options the probe's file
// lacks, answers with a response holding no file, bytes all the
// same: its features, as every generator's framework writes them.
func Probe(ctx context.Context, exe string, silent bool) error {
	req, err := probeRequest()
	if err != nil {
		return err
	}
	exe, cleanup, err := startable(exe)
	if err != nil {
		return err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe)
	cmd.Stdin = bytes.NewReader(req)
	cmd.WaitDelay = 5 * time.Second
	var out, errs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errs
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("probe: %s: %w: %s", exe, err, bytes.TrimSpace(errs.Bytes()))
	}
	if out.Len() == 0 {
		return fmt.Errorf("probe: %s: wrote nothing: %s", exe, bytes.TrimSpace(errs.Bytes()))
	}
	var resp pluginpb.CodeGeneratorResponse
	if err := proto.Unmarshal(out.Bytes(), &resp); err != nil {
		return fmt.Errorf("probe: %s: wrote no plugin response: %w", exe, err)
	}
	if resp.Error != nil {
		return fmt.Errorf("probe: %s: answered the probe with an error: %s", exe, resp.GetError())
	}
	if len(resp.File) == 0 && !silent {
		return fmt.Errorf("probe: %s: answered the probe with no file; a plugin generating only for options the probe's file lacks is marked silent in the catalog", exe)
	}
	return nil
}

// startable is the executable as the host starts it: on windows a
// copy under `.exe` in a temporary directory where the path bears no
// extension, since Go's exec resolves a Windows path by its
// extension and the tree's entrypoint bears none; the path itself
// elsewhere, and on windows where it bears one.
func startable(exe string) (string, func(), error) {
	if runtime.GOOS != "windows" || filepath.Ext(exe) != "" {
		return exe, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "pb-plugins-probe-")
	if err != nil {
		return "", nil, err
	}
	copy := filepath.Join(dir, filepath.Base(exe)+".exe")
	if err := copyFile(exe, copy); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return copy, func() { os.RemoveAll(dir) }, nil
}
