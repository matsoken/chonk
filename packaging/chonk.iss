; Inno Setup script for chonk — per-user install, no elevation.
;
; Build:  ISCC /DAppVersion=0.1.0 packaging\chonk.iss
; Expects the binary at {#BinDir}\chonk.exe and writes the setup .exe to {#OutDir}.

#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif

; Inno derives VersionInfoVersion from AppVersion and rejects anything that is
; not purely numeric, so a prerelease tag like 0.2.0-rc1 needs this passed too.
#ifndef NumericVersion
  #define NumericVersion AppVersion
#endif

#ifndef BinDir
  #define BinDir "..\dist"
#endif

#ifndef OutDir
  #define OutDir "..\dist"
#endif

#define AppName "chonk"
#define AppPublisher "matsoken"
#define AppURL "https://github.com/matsoken/chonk"

[Setup]
; This GUID is the package's identity forever. winget correlates installs by
; ProductCode, which for Inno is "{<AppId>}_is1" — change this and every existing
; install becomes an orphan that upgrades can no longer find.
AppId={{6FCC8C73-B37F-4D6C-BA27-9D8FA369E034}
AppName={#AppName}
AppVersion={#AppVersion}
; Without this, Add/Remove Programs reads "chonk version 0.1.0".
AppVerName={#AppName} {#AppVersion}
VersionInfoVersion={#NumericVersion}
AppPublisher={#AppPublisher}
AppPublisherURL={#AppURL}
AppSupportURL={#AppURL}/issues
AppUpdatesURL={#AppURL}/releases

; chonk needs no admin to run, so its installer must not ask for any either.
; "lowest" keeps the whole install inside the user profile and registers the
; uninstall entry under HKCU.
PrivilegesRequired=lowest
DefaultDirName={localappdata}\Programs\{#AppName}
DefaultGroupName={#AppName}
DisableProgramGroupPage=yes
LicenseFile=..\LICENSE

ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible

OutputDir={#OutDir}
OutputBaseFilename={#AppName}-{#AppVersion}-amd64-setup
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern

; Makes Inno broadcast WM_SETTINGCHANGE after install, so a newly opened shell
; picks up the PATH change without a logoff.
ChangesEnvironment=yes

[Files]
Source: "{#BinDir}\chonk.exe"; DestDir: "{app}"; Flags: ignoreversion

[Code]
const
  EnvKey = 'Environment';

function ReadUserPath(var Value: string): Boolean;
begin
  { RegQueryStringValue hands back REG_EXPAND_SZ data unexpanded, which is what
    we want — rewriting a neighbour's %USERPROFILE% as a literal path would be a
    nasty thing to do to someone's PATH. }
  Result := RegQueryStringValue(HKEY_CURRENT_USER, EnvKey, 'Path', Value);
  if not Result then
    Value := '';
end;

{ Rebuilds PathValue without any segment naming Dir, comparing case-insensitively
  and ignoring surrounding spaces and a trailing backslash. Empty segments are
  dropped too, so repeated install/uninstall cycles cannot silt up the value with
  ';;'. Used on the way in as well as out, which is what makes install idempotent:
  reinstalling strips the old entry before appending, so there is never a second. }
function StripDir(const PathValue, Dir: string): string;
var
  Rest, Seg, Norm, Want: string;
  P: Integer;
begin
  Result := '';
  Want := Uppercase(RemoveBackslashUnlessRoot(Trim(Dir)));
  Rest := PathValue;
  while Rest <> '' do
  begin
    P := Pos(';', Rest);
    if P > 0 then
    begin
      Seg := Trim(Copy(Rest, 1, P - 1));
      Rest := Copy(Rest, P + 1, Length(Rest));
    end
    else
    begin
      Seg := Trim(Rest);
      Rest := '';
    end;

    if Seg <> '' then
    begin
      Norm := Uppercase(RemoveBackslashUnlessRoot(Seg));
      if Norm <> Want then
      begin
        if Result <> '' then
          Result := Result + ';';
        Result := Result + Seg;
      end;
    end;
  end;
end;

procedure AddToUserPath(const Dir: string);
var
  V: string;
begin
  ReadUserPath(V);
  V := StripDir(V, Dir);
  if V <> '' then
    V := V + ';' + Dir
  else
    V := Dir;
  RegWriteExpandStringValue(HKEY_CURRENT_USER, EnvKey, 'Path', V);
end;

procedure RemoveFromUserPath(const Dir: string);
var
  V: string;
begin
  if not ReadUserPath(V) then
    Exit;
  RegWriteExpandStringValue(HKEY_CURRENT_USER, EnvKey, 'Path', StripDir(V, Dir));
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
    AddToUserPath(ExpandConstant('{app}'));
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
    RemoveFromUserPath(ExpandConstant('{app}'));
end;
