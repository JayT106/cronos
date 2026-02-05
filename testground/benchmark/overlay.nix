final: _:
let
  overrides =
    { lib, poetry2nix }:
    let
      customOverrides =
        self: super:
        let
          buildSystems = {
            pystarport = [ "poetry-core" ];
            durations = [ "setuptools" ];
            multitail2 = [ "setuptools" ];
            docker = [
              "hatchling"
              "hatch-vcs"
            ];
            pyunormalize = [ "setuptools" ];
            pytest-github-actions-annotate-failures = [ "setuptools" ];
            cprotobuf = [ "setuptools" ];
            flake8-black = [ "setuptools" ];
            flake8-isort = [ "hatchling" ];
            isort = [ "poetry-core" ];
          };
        in
        (lib.mapAttrs (
          attr: systems:
          super.${attr}.overridePythonAttrs (old: {
            nativeBuildInputs = (old.nativeBuildInputs or [ ]) ++ map (a: self.${a}) systems;
          })
        ) buildSystems)
        // {
          rpds-py = super.rpds-py.overridePythonAttrs (old:
            lib.optionalAttrs (!(old.src.isWheel or false)) {
              cargoDeps = self.pkgs.rustPlatform.fetchCargoVendor {
                inherit (old) src;
                hash = "sha256-dvvqI7l34GZPjBRgTT/WPZk7Ok9stfBtUR1YvNXlCqw=";
              };
            }
          );
        };
    in
    [
      poetry2nix.defaultPoetryOverrides
      customOverrides
    ];

  src =
    nix-gitignore:
    nix-gitignore.gitignoreSourcePure [
      "/*" # ignore all, then add whitelists
      "!/benchmark/"
      "!poetry.lock"
      "!pyproject.toml"
    ] ./.;

  benchmark =
    {
      lib,
      poetry2nix,
      python311,
      nix-gitignore,
    }:
    poetry2nix.mkPoetryApplication {
      projectDir = src nix-gitignore;
      python = python311;
      overrides = overrides { inherit lib poetry2nix; };
    };

  benchmark-env =
    {
      lib,
      poetry2nix,
      python311,
      nix-gitignore,
    }:
    poetry2nix.mkPoetryEnv {
      projectDir = src nix-gitignore;
      python = python311;
      overrides = overrides { inherit lib poetry2nix; };
    };

in
{
  benchmark-testcase = final.callPackage benchmark { };
  benchmark-testcase-env = final.callPackage benchmark-env { };
}
