protoc:
	protoc -I magnet2torrent/ magnet2torrent/magnet2torrent.proto --go_out=plugins=grpc:magnet2torrent