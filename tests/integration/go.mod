module github.com/brokenbots/overlord/tests/integration

go 1.26

require (
	connectrpc.com/connect v1.19.2
	github.com/brokenbots/overlord/shared v0.0.0
	golang.org/x/net v0.28.0
)

require (
	golang.org/x/text v0.17.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

replace github.com/brokenbots/overlord/shared => ../../shared
