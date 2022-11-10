.PHONY: genproto
.SILENT: genproto
genproto:
		protoc --go_out=. --go_opt=paths=source_relative \
             --go-grpc_out=. --go-grpc_opt=paths=source_relative \
             upscale.proto


