PREFIX=/usr/local
BINDIR=${PREFIX}/bin
DESTDIR=
BLDDIR=build
BLDFLAGS=
IMAGE?=bigtable-emulator-extended:latest
GO_OFFLINE?=false
GO_OFFLINE_DEPS_DIR?=.docker/offline-go
EXT=
ifeq (${GOOS},windows)
    EXT=.exe
endif

APPS = bigtable-emulator-extended
all: $(APPS)

$(BLDDIR)/bigtable-emulator-extended:        $(wildcard *.go    bttest/*.go)

$(BLDDIR)/%:
	@mkdir -p $(dir $@)
	go build ${BLDFLAGS} -o $@ .

$(APPS): %: $(BLDDIR)/%

clean:
	rm -fr $(BLDDIR)

docker-build:
	docker build --pull=false \
		--build-arg GO_OFFLINE=${GO_OFFLINE} \
		--build-arg GO_OFFLINE_DEPS_DIR=${GO_OFFLINE_DEPS_DIR} \
		-t ${IMAGE} .

docker-build-offline:
	$(MAKE) docker-build GO_OFFLINE=true

.PHONY: install clean all docker-build docker-build-offline
.PHONY: $(APPS)

install: $(APPS)
	install -m 755 -d ${DESTDIR}${BINDIR}
	for APP in $^ ; do install -m 755 ${BLDDIR}/$$APP ${DESTDIR}${BINDIR}/$$APP${EXT} ; done
