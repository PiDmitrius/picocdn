.PHONY: build install clean

build:
	go build -o picocdn ./cmd/picocdn

install:
	go install ./cmd/picocdn

clean:
	rm -f picocdn
