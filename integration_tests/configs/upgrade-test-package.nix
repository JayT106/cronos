let
  pkgs = import ../../nix { };
  fetchFlake =
    repo: rev:
    (pkgs.flake-compat {
      src = {
        outPath = builtins.fetchTarball "https://github.com/${repo}/archive/${rev}.tar.gz";
        inherit rev;
        shortRev = builtins.substring 0 7 rev;
      };
    }).defaultNix;
  # release/v1.4.8 - used as genesis for upgrade test
  released1_4 =
    (fetchFlake "crypto-org-chain/cronos" "513fda768eb6d0602df1abe48abd4d2cda7a2a11").default;
  # release/v1.5.4
  released1_5 =
    (fetchFlake "crypto-org-chain/cronos" "5ccd423a14f100f4e485d0fb6aa8fa4b96d11b60").default;
  # release/v1.6.1
  released1_6 =
    (fetchFlake "crypto-org-chain/cronos" "05e102ef83b9ab0d5b55d46fb90f5fee53a295d2").default;
  current = pkgs.callPackage ../../. { };
in
pkgs.linkFarm "upgrade-test-package" [
  {
    name = "genesis";
    path = released1_4;
  }
  {
    name = "v1.5";
    path = released1_5;
  }
  {
    name = "v1.6";
    path = released1_6;
  }
  {
    name = "v1.7";
    path = current;
  }
]
