plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

val repoRoot = layout.projectDirectory.dir("../../..")
val generatedSkirkJniLibs = layout.buildDirectory.dir("generated/skirk-go/jniLibs")
val generatedHevJniLibs = layout.buildDirectory.dir("generated/hev-tun2socks/jniLibs")
val hevSourceDir = repoRoot.dir("third_party/hev-socks5-tunnel")
val skirkAppVersion = providers.gradleProperty("skirk.version").orElse("0.1.54").get()
val releaseKeystorePath = providers.environmentVariable("SKIRK_ANDROID_KEYSTORE_FILE").orNull
val releaseKeystorePassword = providers.environmentVariable("SKIRK_ANDROID_KEYSTORE_PASSWORD").orNull
val releaseKeyAlias = providers.environmentVariable("SKIRK_ANDROID_KEY_ALIAS").orNull
val releaseKeyPassword = providers.environmentVariable("SKIRK_ANDROID_KEY_PASSWORD").orNull
val hasReleaseSigning = listOf(
    releaseKeystorePath,
    releaseKeystorePassword,
    releaseKeyAlias,
    releaseKeyPassword,
).all { !it.isNullOrBlank() }
val androidNativeAbis = listOf("arm64-v8a", "armeabi-v7a")
val goAndroidTargets = listOf(
    Triple("arm64-v8a", "arm64", ""),
    Triple("armeabi-v7a", "arm", "7"),
)

val buildSkirkAndroidSidecar = tasks.register("buildSkirkAndroidSidecar") {
    group = "build"
    description = "Build the Skirk Go engine as Android native executables packaged as JNI libs."
    inputs.dir(repoRoot.dir("cmd"))
    inputs.dir(repoRoot.dir("internal"))
    inputs.file(repoRoot.file("go.mod"))
    inputs.property("skirkAppVersion", skirkAppVersion)
    outputs.dir(generatedSkirkJniLibs)

    doLast {
        val sdkRoot = androidSdkRoot()
        val ndkRoot = sdkRoot.resolve("ndk/${android.ndkVersion}")
        check(ndkRoot.exists()) { "Android NDK was not found at ${ndkRoot.absolutePath}" }
        val hostTag = when {
            org.gradle.internal.os.OperatingSystem.current().isLinux -> "linux-x86_64"
            org.gradle.internal.os.OperatingSystem.current().isMacOsX -> "darwin-x86_64"
            org.gradle.internal.os.OperatingSystem.current().isWindows -> "windows-x86_64"
            else -> error("Unsupported host OS for Android NDK clang")
        }
        val toolchainBin = ndkRoot.resolve("toolchains/llvm/prebuilt/$hostTag/bin")
        val clangSuffix = if (org.gradle.internal.os.OperatingSystem.current().isWindows) ".cmd" else ""
        val cCompilers = mapOf(
            "arm64-v8a" to toolchainBin.resolve("aarch64-linux-android26-clang$clangSuffix").absolutePath,
            "armeabi-v7a" to toolchainBin.resolve("armv7a-linux-androideabi26-clang$clangSuffix").absolutePath,
        )
        goAndroidTargets.forEach { (abi, goArch, goArm) ->
            val outputDir = generatedSkirkJniLibs.get().dir(abi).asFile
            outputDir.mkdirs()
            exec {
                workingDir = repoRoot.asFile
                executable = "go"
                args(
                    "build",
                    "-trimpath",
                    "-buildmode=pie",
                    "-ldflags",
                    "-s -w -X main.version=android-$skirkAppVersion",
                    "-o",
                    outputDir.resolve("libskirk.so").absolutePath,
                    "./cmd/skirk",
                )
                environment("GOOS", "android")
                environment("GOARCH", goArch)
                if (goArm.isNotBlank()) {
                    environment("GOARM", goArm)
                }
                environment("CGO_ENABLED", "1")
                environment("CC", requireNotNull(cCompilers[abi]) { "missing Android compiler for $abi" })
            }
        }
    }
}

fun androidSdkRoot(): File {
    val explicit = providers.gradleProperty("android.sdk.path").orNull
    val env = System.getenv("ANDROID_HOME") ?: System.getenv("ANDROID_SDK_ROOT")
    val local = rootProject.file("local.properties")
        .takeIf { it.exists() }
        ?.readLines()
        ?.firstOrNull { it.startsWith("sdk.dir=") }
        ?.substringAfter("sdk.dir=")
    return File(explicit ?: env ?: local ?: error("Android SDK path was not found"))
}

val buildHevTun2socks = tasks.register("buildHevTun2socks") {
    group = "build"
    description = "Build the Android TUN-to-SOCKS bridge used by VPN mode."
    inputs.dir(hevSourceDir)
    outputs.dir(generatedHevJniLibs)

    doLast {
        val sdkRoot = androidSdkRoot()
        val ndkBuild = sdkRoot.resolve("ndk/${android.ndkVersion}/ndk-build")
        check(ndkBuild.exists()) { "ndk-build was not found at ${ndkBuild.absolutePath}" }

        val appMk = temporaryDir.resolve("SkirkApplication.mk")
        appMk.writeText(
            """
            APP_PLATFORM := android-26
            APP_OPTIM := release
            APP_ABI := ${androidNativeAbis.joinToString(" ")}
            APP_CFLAGS := -O3 -DPKGNAME=app/skirk/client -DCLSNAME=HevTun2Socks
            APP_SUPPORT_FLEXIBLE_PAGE_SIZES := true
            NDK_TOOLCHAIN_VERSION := clang
            """.trimIndent() + "\n",
        )

        exec {
            environment("ANDROID_HOME", sdkRoot.absolutePath)
            environment("ANDROID_SDK_ROOT", sdkRoot.absolutePath)
            workingDir = hevSourceDir.asFile
            commandLine(
                ndkBuild.absolutePath,
                "NDK_PROJECT_PATH=.",
                "NDK_APPLICATION_MK=${appMk.absolutePath}",
                "APP_BUILD_SCRIPT=${hevSourceDir.file("Android.mk").asFile.absolutePath}",
                "V=0",
            )
        }

        androidNativeAbis.forEach { abi ->
            val outputDir = generatedHevJniLibs.get().dir(abi).asFile
            outputDir.mkdirs()
            hevSourceDir.file("libs/$abi/libhev-socks5-tunnel.so").asFile
                .copyTo(outputDir.resolve("libhev-socks5-tunnel.so"), overwrite = true)
        }
    }
}

android {
    namespace = "app.skirk.client"
    compileSdk = 35
    ndkVersion = "27.0.12077973"

    defaultConfig {
        applicationId = "app.skirk.client"
        minSdk = 26
        targetSdk = 35
        versionCode = 54
        versionName = skirkAppVersion

        ndk {
            abiFilters += androidNativeAbis
        }
    }

    splits {
        abi {
            isEnable = true
            reset()
            include(*androidNativeAbis.toTypedArray())
            isUniversalApk = true
        }
    }

    signingConfigs {
        if (hasReleaseSigning) {
            create("release") {
                storeFile = file(requireNotNull(releaseKeystorePath))
                storePassword = releaseKeystorePassword
                keyAlias = releaseKeyAlias
                keyPassword = releaseKeyPassword
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            isShrinkResources = false
            if (hasReleaseSigning) {
                signingConfig = signingConfigs.getByName("release")
            }
        }
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    sourceSets {
        getByName("main") {
            jniLibs.srcDir(generatedSkirkJniLibs)
            jniLibs.srcDir(generatedHevJniLibs)
        }
    }

    packaging {
        jniLibs {
            useLegacyPackaging = true
        }
    }
}

tasks.named("preBuild") {
    dependsOn(buildSkirkAndroidSidecar)
    dependsOn(buildHevTun2socks)
}

dependencies {
    val composeBom = platform("androidx.compose:compose-bom:2024.12.01")
    implementation(composeBom)
    implementation("androidx.activity:activity-compose:1.9.3")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.material:material-icons-extended")
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.ui:ui-tooling-preview")
    debugImplementation("androidx.compose.ui:ui-tooling")
}
