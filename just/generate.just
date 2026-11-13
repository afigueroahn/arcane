import 'settings.just'

set working-directory := '..'

[default]
[private]
list:
    @{{ just_executable() }} --justfile {{ justfile() }} --list generate

# Agent tunnel protobuf/gRPC code
proto:
    {{ just_executable() }} lint proto
    cd {{ tunnel_proto_dir }} && ../.bin/buf generate

# OpenAPI spec and frontend API types
openapi:
    go run -tags exclude_frontend ./backend/cmd openapi --output frontend/openapi.yaml
    vp -C frontend run openapi
