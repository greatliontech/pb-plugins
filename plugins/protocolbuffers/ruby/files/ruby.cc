// protoc-gen-ruby: protobuf's ruby generator run through the plugin
// protocol, the plugin executable protobuf does not ship.
#include <google/protobuf/compiler/ruby/ruby_generator.h>
#include <google/protobuf/compiler/plugin.h>

int main(int argc, char *argv[]) {
  google::protobuf::compiler::ruby::Generator generator;
  return google::protobuf::compiler::PluginMain(argc, argv, &generator);
}
