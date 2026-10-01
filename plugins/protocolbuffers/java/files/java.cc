// protoc-gen-java: protobuf's java generator run through the plugin
// protocol, the plugin executable protobuf does not ship.
#include <google/protobuf/compiler/java/generator.h>
#include <google/protobuf/compiler/plugin.h>

int main(int argc, char *argv[]) {
  google::protobuf::compiler::java::JavaGenerator generator;
  return google::protobuf::compiler::PluginMain(argc, argv, &generator);
}
