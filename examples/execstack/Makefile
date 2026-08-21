# Modeled on the remote-make example. Changing PROG and CFLAGS is enough for
# most single-binary challenges.

PROG = execstack
FLAG ?= flag{dummy_value} # Only set if not defined in the environment
FLAG_FILE ?= flag.txt

CFLAGS = -O0 -g -m32

.PHONY: all
all: main artifacts.tar.gz metadata.json

.PHONY: main
main: $(PROG) $(FLAG_FILE)

.PHONY: run
run: main
	@./$(PROG)

artifacts.tar.gz: $(PROG).c
	tar czvf $@ $^

metadata.json:
	@echo "{\"flag\":\"$(FLAG)\"}" > metadata.json

$(FLAG_FILE):
	@echo -n $(FLAG) > $(FLAG_FILE)

clean:
	rm -f $(PROG) $(FLAG_FILE) artifacts.tar.gz metadata.json
