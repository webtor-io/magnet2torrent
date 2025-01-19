protoc:
	protoc proto/magnet2torrent.proto --go_out=. --go_opt=paths=source_relative \
		   --go-grpc_out=. --go-grpc_opt=paths=source_relative proto/magnet2torrent.proto