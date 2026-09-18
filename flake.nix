{
  description = "Linux namespace sandbox for pi";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
    in
    {
      packages = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.buildGoModule rec {
            pname = "pi-square";
            version = "0.0.0-preview.v${pkgs.lib.removeSuffix "-dirty" (self.shortRev or self.dirtyShortRev or "unknown")}";

            src = self;
            vendorHash = "sha256-ICVUFyzXUrXHfLYdveWnxaLK/DaDauo6swCaYNDNhbc=";

            subPackages = [ "." ];
            ldflags = [ "-X main.version=${version}" ];

            meta = {
              description = "Linux namespace sandbox for pi";
              homepage = "https://github.com/lobkovilya/pi-square";
              mainProgram = "pi-square";
              platforms = nixpkgs.lib.platforms.linux;
            };
          };
        });

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/pi-square";
        };
      });

      formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.nixfmt-tree);
    };
}
