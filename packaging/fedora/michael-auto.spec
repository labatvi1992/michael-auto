Name:           michael-auto
Version:        2.5.0
Release:        1%{?dist}
Summary:        Evidence-Driven AI Quota & Profile Orchestrator for Fedora Wayland (trycua/cua)

License:        MIT
URL:            https://github.com/labatvi1992/michael-auto
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  golang >= 1.24
BuildRequires:  nodejs >= 20
BuildRequires:  npm
BuildRequires:  gtk3-devel
BuildRequires:  webkit2gtk4.1-devel
BuildRequires:  pkgconfig

Requires:       gtk3
Requires:       webkit2gtk4.1
Requires:       python3
Requires:       glib2
Requires:       at-spi2-core

%description
Michael Auto monitors Antigravity quota inside a dedicated Fedora Wayland
"Agent Workspace" via trycua/cua (cua-driver 0.58.1), enforces Verify ->
Screenshot Evidence (SHA-256 + HiDPI editor privacy redaction) -> Execute,
30-second sliding Human Yield, GNOME Lock Screen pause, and strict
1-Account-Per-Chrome-Profile OAuth2 PKCE routing without storing passwords
or OAuth tokens.

%prep
%autosetup

%build
pushd frontend
npm ci
npm run build
popd
go build -tags webkit2_41 -trimpath -ldflags="-s -w" -o build/bin/michael-auto .

%install
install -Dpm 0755 build/bin/michael-auto %{buildroot}%{_bindir}/michael-auto
install -Dpm 0644 packaging/michael-auto.desktop %{buildroot}%{_datadir}/applications/michael-auto.desktop
install -Dpm 0644 packaging/icons/michael-auto.svg %{buildroot}%{_datadir}/icons/hicolor/scalable/apps/michael-auto.svg
sed 's|%h/.local/bin/michael-auto|%{_bindir}/michael-auto|g' packaging/michael-auto.service > michael-auto.service.rpm
install -Dpm 0644 michael-auto.service.rpm %{buildroot}%{_userunitdir}/michael-auto.service

%files
%license LICENSE
%doc README.md docs/ARCHITECTURE.md
%{_bindir}/michael-auto
%{_datadir}/applications/michael-auto.desktop
%{_datadir}/icons/hicolor/scalable/apps/michael-auto.svg
%{_userunitdir}/michael-auto.service

%post
echo "Run 'michael-auto --setup-sidecar && michael-auto --doctor' as your desktop user to initialize Agent Workspace & Python venv."

%changelog
* Thu Nov 26 2026 Michael Auto Maintainers <maintainers@michael-auto.local> - 2.5.0-1
- Production Release v2.5.0: Dedicated Agent Workspace, offscreen Meta.WindowActor capture, HiDPI redaction, GNOME LockScreen guard, pinned cua-driver==0.58.1, Dual-Track RPM + systemd packaging.
