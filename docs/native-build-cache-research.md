# Expo and iOS build caching research

Checked 2026-09-12 against Expo, Apple, and Swift primary sources. These are proposed directions for Parle, Booker, and a new phone app, not implemented Layer Cache integrations or measured savings.

## Avoid scheduling native builds first

Expo Fingerprint hashes native dependencies, custom native code, project files, and configuration. Its standalone Node package can calculate separate iOS and Android fingerprints. Use a matching fingerprint to reuse an existing native runtime. Preserve the fingerprint inputs for explaining misses, and run fingerprinting with the same environment as the eventual build. [Fingerprint API](https://docs.expo.dev/versions/v55.0.0/sdk/fingerprint/), [environment consistency](https://docs.expo.dev/eas/environment-variables/usage/)

For development, build and install the native client once, then run `npx expo start` for JavaScript changes. For installed release apps, publish compatible JavaScript and assets through `expo-updates`. Both routes avoid native compilation for compatible changes. Native dependencies and config plugins can require another build. [Local development](https://docs.expo.dev/guides/local-app-development/), [EAS Update setup](https://docs.expo.dev/eas-update/getting-started/)

For CI artifacts, standalone `@expo/repack-app` replaces JavaScript and assets inside an existing APK, IPA, or simulator `.app`. Check fingerprint compatibility first. Simulator artifacts need no signing; physical-device artifacts need signing credentials. iOS repack supports development and ad-hoc signing only. Expo advises complete production build pipelines for store submissions because of signing and symbolication. Repack therefore fits preview and smoke-test artifacts, not a blanket replacement for TestFlight/App Store releases. The reviewed documentation does not establish that every iOS repack mode runs on Linux. Keep iOS signing on the Mac until the pinned tool is tested. [Repack reference](https://docs.expo.dev/build-reference/repack/)

Proposed cache identity: app + platform + simulator/device + build profile/configuration + native fingerprint + toolchain identity. Track signing/provisioning compatibility separately. A native fingerprint establishes native compatibility, not the identity of the freshly bundled JavaScript. Store the JS commit/hash with each repacked artifact. These extra identity rules are design recommendations, not a documented Expo cache-key contract.

## Replace hosted build execution without confusing it with caching

`eas build --local` runs the build process on owned infrastructure, but Expo explicitly does not support its cloud caching feature locally. It also ignores tool-version/image fields in `eas.json`, needs locally supplied secret environment variables, and requires Expo authentication. Its EAS communication includes project existence checks and managed credential downloads when used. Pin/install the local tools yourself. [Local EAS Build limits](https://docs.expo.dev/build-reference/local-builds/)

iOS compilation still uses Xcode; Android compilation uses the Android SDK. A persistent Mac can handle native iOS misses while Linux performs fingerprinting, JS work, Android work, and artifact storage. This division is an architectural recommendation based on the tools' platform requirements, not Linux execution of Xcode. [Expo local compilation](https://docs.expo.dev/guides/local-app-development/), [Expo build infrastructure](https://docs.expo.dev/build-reference/infrastructure/)

Expo also has a custom build-cache-provider API with lookup and upload hooks. This is a promising small Layer Cache integration for local developer builds. It applies only to `npx expo run:android` and `run:ios`, does not intercept `eas build`, and excludes iOS physical-device targets because provisioning limits artifact reuse. Remote EAS build numbers are not fingerprint inputs; cached binaries retain their original numbers. [Build cache providers](https://docs.expo.dev/guides/cache-builds-remotely/)

## Make native misses cheaper

Before building another cache protocol, check whether the app can consume precompiled dependencies. Expo documents precompiled React Native dependencies from RN 0.80 and core from RN 0.81 with the new architecture, plus precompiled Expo modules in current SDKs. `expo-build-properties` exposes `ccacheEnabled` for iOS C++ compilation. These options depend on the app's SDK/RN version and native integration; current documentation defaults do not prove they are enabled in these repositories. ccache does not establish Swift cache coverage. [Build properties](https://docs.expo.dev/versions/latest/sdk/build-properties/)

Xcode 26 introduced opt-in compilation caching for Swift and C-family languages through its Enable Compilation Caching setting. Apple specifically identifies branch switching and clean builds as useful cases. Start with this on a persistent Mac, preserve its cache, and measure compiler cache hits and total build duration. Compilation reuse does not eliminate packaging, linking, signing, or simulator execution. [Xcode 26 release notes](https://developer.apple.com/documentation/Xcode-Release-Notes/xcode-26-release-notes)

A remote Swift/Clang cache is technically credible but needs a separate adapter. Swift's LLVM fork publishes CAS and key/value gRPC definitions. This is a concrete protocol to investigate, not evidence that existing Turbo, Actions, or Docker cache endpoints accept Xcode traffic. [Remote cache protocol source](https://github.com/swiftlang/llvm-project/tree/next/llvm/lib/RemoteCachingService/RemoteCacheProto)

SwiftPM proposal SE-0547 describes the LLVM CAS plugin, remote service socket, and path prefix mapping. The source still labels it Active Review, so its proposed command-line interface must not be presented as a released feature. Swift compiler maintainers have also described absolute-path normalization as necessary for distributed reuse. Prototype against one pinned Xcode version before committing to support; test equivalent projects in different checkout paths and across Macs. [SE-0547](https://github.com/swiftlang/swift-evolution/blob/main/proposals/0547-swiftpm-compilation-caching.md), [maintainer discussion of path mapping](https://forums.swift.org/t/about-swift-shared-cache-across-machines/81850)

## EAS Update can also be replaced, separately

`expo-updates` accepts a custom `updates.url` and `runtimeVersion`. Its open HTTP protocol specifies platform/runtime matching, manifests, assets, directives, and response headers. A Linux service can implement this protocol, but artifact storage alone is insufficient. [Client configuration](https://docs.expo.dev/versions/latest/sdk/updates/), [Expo Updates protocol](https://docs.expo.dev/technical-specs/expo-updates-1/)

Expo's example server explicitly offers no guarantee of completeness, stability, or production performance. A production replacement needs its own publishing, runtime selection, rollback, signing, and operational validation. Retaining EAS Update initially while replacing EAS Build is a reasonable smaller migration. [Official example server caveats](https://github.com/expo/custom-expo-updates-server)

## Suggested order

The parent investigation inspected these local pipelines. Booker already calculates a fingerprint on Linux before starting macOS, stores native artifacts by fingerprint, and restores Pods/DerivedData. Its remote simulator script injects current Hermes JavaScript on Linux, but resets Metro's cache and copies assets best-effort. Preserve the existing gate, make bundle/asset replacement strict, and compare the pinned Expo repack tool before replacing that script. Sources are Booker's `.github/workflows/mobile-native.yml` and `scripts/mobile/ios-remote.sh`.

Parle starts its Safari job on macOS for every run. Turbo covers `package:safari`, but the separate macOS/iOS `xcodebuild` commands execute outside that cached task and keep DerivedData under `RUNNER_TEMP`. Add a Linux artifact lookup before scheduling macOS, then preserve native caches for misses. Source is Parle's `.github/workflows/ci.yml`. Safari extension artifacts need a full output key that includes bundled JS/assets; a native-runtime fingerprint alone is insufficient.

The new phone app uses Expo 57 and local Swift/Kotlin modules under `apps/mobile/modules/phone-native`. It has development, preview, and production EAS profiles but no configured `runtimeVersion` or `expo-updates`. Establish native fingerprint inputs, a reusable development client, and update compatibility configuration before expecting OTA to skip release builds. Sources are its `.github/workflows/check.yml`, `eas.json`, and app configuration. The inspected checkout has no Git remote, so CI activation also remains an operational step.

1. Finish build skipping and strict artifact reuse in these pipelines. Reuse development clients, and publish compatible updates once configured.
2. Run native iOS misses on one persistent Mac. Verify precompiled dependencies and local compiler caching, and record cold build, warm build, repack, and skipped-job timings.
3. Add a Layer Cache Expo artifact provider if developer reuse justifies it. Treat remote Xcode CAS and self-hosted updates as independent follow-up projects, each with a working compatibility test before rollout.
