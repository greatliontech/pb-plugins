// protoc-gen-python: protobuf's python generator run through the plugin
// protocol, the plugin executable protobuf does not ship.
#include <google/protobuf/compiler/python/generator.h>
#include <google/protobuf/compiler/plugin.h>

int main(int argc, char *argv[]) {
  google::protobuf::compiler::python::Generator generator;
  return google::protobuf::compiler::PluginMain(argc, argv, &generator);
}
