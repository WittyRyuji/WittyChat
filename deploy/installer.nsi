; WittyChat NSIS installer template — builds wittychat-setup.exe.
; Checks WebView2 runtime; bootstraps it silently when missing.
;
; makensis installer.nsi  (after `wails3 build` produced bin/wittychat.exe)

!include "MUI2.nsh"
!include "LogicLib.nsh"

Name "WittyChat"
OutFile "wittychat-setup.exe"
InstallDir "$PROGRAMFILES64\WittyChat"
RequestExecutionLevel admin

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_LANGUAGE "English"
!insertmacro MUI_LANGUAGE "SimpChinese"

Section "Install"
    SetOutPath "$INSTDIR"
    File "..\bin\wittychat.exe"

    ; WebView2 runtime check: registry key exists when installed.
    ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
    ${If} $0 == ""
        DetailPrint "WebView2 missing — downloading evergreen bootstrapper…"
        NSISdl::download "https://go.microsoft.com/fwlink/p/?LinkId=2124703" "$TEMP\WebView2Setup.exe"
        ExecWait '"$TEMP\WebView2Setup.exe" /silent /install'
    ${EndIf}

    WriteUninstaller "$INSTDIR\uninstall.exe"
    CreateShortcut "$DESKTOP\WittyChat.lnk" "$INSTDIR\wittychat.exe"
    CreateDirectory "$SMPROGRAMS\WittyChat"
    CreateShortcut "$SMPROGRAMS\WittyChat\WittyChat.lnk" "$INSTDIR\wittychat.exe"
SectionEnd

Section "Uninstall"
    Delete "$INSTDIR\wittychat.exe"
    Delete "$INSTDIR\uninstall.exe"
    Delete "$DESKTOP\WittyChat.lnk"
    RMDir /r "$SMPROGRAMS\WittyChat"
    RMDir "$INSTDIR"
SectionEnd
