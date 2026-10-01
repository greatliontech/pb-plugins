// protoc-gen-php: protobuf's php generator run through the plugin
// protocol, the plugin executable protobuf does not ship.
#include <google/protobuf/compiler/php/php_generator.h>
#include <google/protobuf/compiler/plugin.h>

int main(int argc, char *argv[]) {
  google::protobuf::compiler::php::Generator generator;
  return google::protobuf::compiler::PluginMain(argc, argv, &generator);
}
