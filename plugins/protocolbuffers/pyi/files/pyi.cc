// protoc-gen-pyi: protobuf's pyi generator run through the plugin
// protocol, the plugin executable protobuf does not ship.
#include <google/protobuf/compiler/python/pyi_generator.h>
#include <google/protobuf/compiler/plugin.h>

int main(int argc, char *argv[]) {
  google::protobuf::compiler::python::PyiGenerator generator;
  return google::protobuf::compiler::PluginMain(argc, argv, &generator);
}
