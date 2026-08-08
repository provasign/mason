#!/usr/bin/env bash
# Generate the Homebrew formula for a released mason version.
#
# The tap drifted 15 releases behind (0.23.0 while v0.38.0 shipped) because
# nothing updated it: mason builds its release assets by hand rather than
# through GoReleaser, so it never had the tap-publish step shale has. This
# script is that step, callable from CI or by hand.
#
# Usage: scripts/gen-homebrew-formula.sh v0.38.0 [checksums.txt]
set -euo pipefail

ver="${1:?usage: $0 <vX.Y.Z> [checksums.txt]}"
num="${ver#v}"
sums="${2:-}"

if [ -z "${sums}" ]; then
  sums="$(mktemp)"
  gh release download "${ver}" --repo provasign/mason --pattern "checksums.txt" \
     --output "${sums}" --clobber
fi

sha() { grep -E "mason-${ver}-$1\$" "${sums}" | awk '{print $1}'; }

da="$(sha darwin-amd64)"; dr="$(sha darwin-arm64)"
la="$(sha linux-amd64)";  lr="$(sha linux-arm64)"
for v in "$da" "$dr" "$la" "$lr"; do
  [ -n "$v" ] || { echo "missing a checksum for ${ver}" >&2; exit 1; }
done

base="https://github.com/provasign/mason/releases/download/${ver}"
cat <<FORMULA
# typed: false
# frozen_string_literal: true

class Mason < Formula
  desc "Model-agnostic coding agent with the prism code graph baked in"
  homepage "https://github.com/provasign/mason"
  version "${num}"
  license "Apache-2.0"

  on_macos do
    if Hardware::CPU.intel?
      url "${base}/mason-${ver}-darwin-amd64"
      sha256 "${da}"

      define_method(:install) do
        bin.install "mason-${ver}-darwin-amd64" => "mason"
      end
    end
    if Hardware::CPU.arm?
      url "${base}/mason-${ver}-darwin-arm64"
      sha256 "${dr}"

      define_method(:install) do
        bin.install "mason-${ver}-darwin-arm64" => "mason"
      end
    end
  end

  on_linux do
    if Hardware::CPU.intel? && Hardware::CPU.is_64_bit?
      url "${base}/mason-${ver}-linux-amd64"
      sha256 "${la}"

      define_method(:install) do
        bin.install "mason-${ver}-linux-amd64" => "mason"
      end
    end
    if Hardware::CPU.arm? && Hardware::CPU.is_64_bit?
      url "${base}/mason-${ver}-linux-arm64"
      sha256 "${lr}"

      define_method(:install) do
        bin.install "mason-${ver}-linux-arm64" => "mason"
      end
    end
  end

  test do
    assert_match "mason", shell_output("#{bin}/mason version")
  end
end
FORMULA
