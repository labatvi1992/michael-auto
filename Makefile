WAILS ?= $(shell command -v wails 2>/dev/null || echo $(HOME)/go/bin/wails)
BUILD_TAGS ?= webkit2_41
PYTHON ?= $(shell command -v python3.12 2>/dev/null || command -v python3.13 2>/dev/null || command -v python3 2>/dev/null || command -v python 2>/dev/null)
SIDECAR_VENV := sidecar/.venv
VERSION := 2.5.0

.PHONY: all deps frontend sidecar test test-race build verify clean install-desktop install-systemd uninstall release-tarball rpm-source

all: build

deps:
	go mod tidy
	cd frontend && npm install

frontend:
	cd frontend && npm run build

sidecar:
	$(PYTHON) -m venv $(SIDECAR_VENV)
	$(SIDECAR_VENV)/bin/python -m ensurepip --upgrade >/dev/null 2>&1 || true
	$(SIDECAR_VENV)/bin/python -m pip install --only-binary=:all: --require-hashes -r sidecar/cua-driver-0.58.1.requirements.lock
	$(SIDECAR_VENV)/bin/python -m pip install --no-deps ./sidecar

test:
	go vet ./...
	go test -v ./internal/...
	python3 -m unittest discover -s sidecar/tests -v
	cd frontend && npm run test -- --run

test-race:
	go vet ./...
	go test -count=1 -race -coverprofile=coverage.out ./internal/...
	go tool cover -func=coverage.out | tail -n 15

build: frontend
	go build -tags $(BUILD_TAGS) -trimpath -o build/bin/michael-auto .

verify: frontend test-race build
	python3 -m unittest discover -s sidecar/tests -v
	cd frontend && npm run test -- --run
	./build/bin/michael-auto --version
	./build/bin/michael-auto --self-test

install-desktop: build
	mkdir -p $(HOME)/.local/bin $(HOME)/.local/share/applications $(HOME)/.local/share/icons/hicolor/scalable/apps
	install -m 0755 ./build/bin/michael-auto $(HOME)/.local/bin/michael-auto
	install -m 0644 packaging/icons/michael-auto.svg $(HOME)/.local/share/icons/hicolor/scalable/apps/michael-auto.svg
	sed "s|Exec=michael-auto|Exec=$(HOME)/.local/bin/michael-auto|g" packaging/michael-auto.desktop > $(HOME)/.local/share/applications/michael-auto.desktop
	gtk-update-icon-cache -q -t -f $(HOME)/.local/share/icons/hicolor 2>/dev/null || true
	update-desktop-database $(HOME)/.local/share/applications 2>/dev/null || true

install-systemd: install-desktop
	mkdir -p $(HOME)/.config/systemd/user
	install -m 0644 packaging/michael-auto.service $(HOME)/.config/systemd/user/michael-auto.service
	systemctl --user daemon-reload || true
	@echo "Đã cài systemd user unit. Bật chạy nền bằng: systemctl --user enable --now michael-auto.service"

uninstall:
	if [ -x ./build/bin/michael-auto ]; then ./build/bin/michael-auto --uninstall; elif command -v michael-auto >/dev/null 2>&1; then michael-auto --uninstall; fi
	rm -f $(HOME)/.local/bin/michael-auto

release-tarball: build
	mkdir -p dist
	tar -czf dist/michael-auto-$(VERSION)-linux-amd64.tar.gz -C build/bin michael-auto -C ../../packaging michael-auto.desktop michael-auto.service icons/michael-auto.svg
	sha256sum dist/michael-auto-$(VERSION)-linux-amd64.tar.gz > dist/SHA256SUMS.txt

rpm-source: frontend
	mkdir -p dist
	rm -rf dist/michael-auto-$(VERSION)
	mkdir -p dist/michael-auto-$(VERSION)
	git archive HEAD | tar -x -C dist/michael-auto-$(VERSION)
	cp -r frontend/dist dist/michael-auto-$(VERSION)/frontend/dist
	(cd dist/michael-auto-$(VERSION) && go mod vendor)
	tar -czf dist/michael-auto-$(VERSION)-source.tar.gz -C dist michael-auto-$(VERSION)
	rm -rf dist/michael-auto-$(VERSION)
	sha256sum dist/michael-auto-$(VERSION)-source.tar.gz >> dist/SHA256SUMS.txt

clean:
	rm -rf build/bin dist frontend/dist coverage.out $(SIDECAR_VENV) sidecar/*.egg-info sidecar/dist
