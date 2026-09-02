.PHONY: build test run clean

build:
	CGO_ENABLED=0 go build -o dolmen .

test:
	go vet ./... && go test ./...

run:
	go run . -addr 127.0.0.1:8790 -data ./data

clean:
	rm -f dolmen
