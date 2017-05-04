format:
	cd example && gofmt -s -w .
	cd selfupdate && gofmt -s -w .
	gofmt -s -w ./main.go
	gofmt -s -w ./main_test.go

install: format
	go install -v


.PHONY: format install

