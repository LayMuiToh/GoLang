#
#

APPNAME  := wss_audio_test
SOURCES  := $(wildcard *.go)

all: $(APPNAME) 

$(APPNAME): $(SOURCES)
	go get github.com/gorilla/websocket
	go build

.PHONY: clean

clean:
	$(RM) $(APPNAME) 

run:
	./$(APPNAME) 
