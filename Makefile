# wristkit 构建入口
#   make app        构建 macOS App：server/app/build/bin/wristkit.app（app 的编译、打包都在 App 里，不用 Node 和小米的工具）
#   make cli        构建命令行版：server/miband
#   make daemon     构建不带窗口的后台版：server/app/build/bin/wristkit（wristkit serve / wristkit gerdr，设置页走 HTTP）
#   make install-daemon  构建后台版并装到 ~/.local/bin/wristkit（gerdr 插件 addons/wristkit 找的就是它）
#   make test       跑 Go 测试
#   make release VERSION=0.1.0   构建 Intel / Apple 芯片通用的 App，打成 dist/wristkit-0.1.0-mac.zip（CI 发版用）

WAILS ?= $(shell command -v wails 2>/dev/null || echo $(HOME)/go/bin/wails)

.PHONY: app cli test release daemon install-daemon

# 本机签名证书（名字见 SIGN_IDENTITY）。macOS 按“标识 + 证书”记蓝牙授权：
# 不签名的话每次构建签名都变，每次都会重新弹蓝牙授权。证书怎么建见 README。
# 证书名和 App 标识（dev.bysir.miband-pet）都沿用改名之前的：证书是钥匙串里已经建好的那张，
# 标识改了 macOS 会当成新 App，要重新授权蓝牙
# 发版（CI）用另一张固定的证书「wristkit release signing」，见 .github/workflows/release.yml。
SIGN_IDENTITY ?= miband pet local signing
# 设了就必须签上名，找不到证书直接失败（发版用）
REQUIRE_SIGN ?=
# App 的版本号，写进 Info.plist；不设就是 Wails 默认的
VERSION ?=
# 目标平台，如 darwin/universal（Intel + Apple 芯片）；不设就是本机的
PLATFORM ?=
APP_BUNDLE = server/app/build/bin/wristkit.app

app:
	cd server/app && $(WAILS) build -clean $(if $(PLATFORM),-platform $(PLATFORM))
	@if [ -n "$(VERSION)" ]; then \
		/usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $(VERSION)" -c "Set :CFBundleVersion $(VERSION)" "$(APP_BUNDLE)/Contents/Info.plist"; \
	fi
	@if security find-identity -p codesigning | grep -q "$(SIGN_IDENTITY)"; then \
		codesign --force --deep --identifier dev.bysir.miband-pet -s "$(SIGN_IDENTITY)" "$(APP_BUNDLE)"; \
	elif [ -n "$(REQUIRE_SIGN)" ]; then \
		echo "没找到签名证书「$(SIGN_IDENTITY)」"; exit 1; \
	else \
		echo "没找到签名证书「$(SIGN_IDENTITY)」，App 未签名：每次构建后都会重新弹蓝牙授权"; \
	fi

# 用 ditto 打包才能保住签名和扩展属性（zip 命令会弄坏）
release:
	@if [ -z "$(VERSION)" ]; then echo "要指定版本号：make release VERSION=0.1.0"; exit 1; fi
	$(MAKE) app PLATFORM=darwin/universal REQUIRE_SIGN=1 VERSION=$(VERSION)
	mkdir -p dist
	ditto -c -k --keepParent "$(APP_BUNDLE)" dist/wristkit-$(VERSION)-mac.zip

cli:
	cd server && go build -o miband ./cmd/miband

# 后台版和 App 是同一份代码（server/app），只是不带 Wails 的构建标签，不开窗口。
# 它不在 .app 包里，Info.plist（标识、蓝牙用途）嵌进二进制的 __TEXT,__info_plist；
# 标识和签名证书都和 App 一样，macOS 把它和 App 当成同一个程序，蓝牙授权共用。
DAEMON = server/app/build/bin/wristkit
daemon:
	cd server/app && $(WAILS) generate module
	cd server/app/frontend && npm install --no-audit --no-fund && npm run build
	cd server/app && go build -ldflags '-s -w -linkmode=external -extldflags "-Wl,-sectcreate,__TEXT,__info_plist,$(CURDIR)/server/app/build/darwin/Info.daemon.plist"' -o build/bin/wristkit .
	@if security find-identity -p codesigning | grep -q "$(SIGN_IDENTITY)"; then \
		codesign --force --identifier dev.bysir.miband-pet -s "$(SIGN_IDENTITY)" "$(DAEMON)"; \
	else \
		echo "没找到签名证书「$(SIGN_IDENTITY)」，未签名：每次构建后都会重新弹蓝牙授权"; \
	fi

install-daemon: daemon
	mkdir -p $(HOME)/.local/bin
	cp $(DAEMON) $(HOME)/.local/bin/wristkit.new && mv $(HOME)/.local/bin/wristkit.new $(HOME)/.local/bin/wristkit

# server/app 依赖构建好的前端（go:embed frontend/dist），由 make app 构建，这里不测
test:
	cd server && go test ./internal/... ./cmd/...
