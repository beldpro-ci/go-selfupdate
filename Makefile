VERSION			:=	$(shell cat ./VERSION)-$(shell git rev-parse --short HEAD)
LD_FLAGS		:= 	-ldflags "-w -s -X main.Version=$(VERSION)"

format:
	cd example && gofmt -s -w .
	cd selfupdate && gofmt -s -w .
	gofmt -s -w ./main.go

install: format
	go build $(LD_FLAGS) -v
	go install $(LD_FLAGS) -v

example:
	cd ./example/src/hello-updater &&  go build -v

.PHONY: format install example

