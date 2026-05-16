plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

val repoRoot = layout.projectDirectory.dir("../../..")
val generatedSkirkJniLibs = layout.buildDirectory.dir("generated/skirk-go/jniLibs")
val generatedHevJniLibs = layout.buildDirectory.dir("generated/hev-tun2socks/jniLibs")
val hevSourceDir = repoRoot.dir("third_party/hev-socks5-tunnel")
val skirkAppVersion = providers.gradleProperty("skirk.version").orElse("0.1.33").get()

val buildSkirkAndroidSidecar = tasks.register("buildSkirkAndroidSidecar") {
    group = "build"
    description = "Build the Skirk Go engine as Android native executables packaged as JNI libs."
    inputs.dir(repoRoot.dir("cmd"))
    inputs.dir(repoRoot.dir("internal"))
    inputs.file(repoRoot.file("go.mod"))
    inputs.property("skirkAppVersion", skirkAppVersion)
    outputs.dir(generatedSkirkJniLibs)

    doLast {
        val targets = listOf(
            Triple("arm64-v8a", "arm64", "libskirk.so"),
        )
        targets.forEach { (abi, goArch, fileName) ->
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
                    outputDir.resolve(fileName).absolutePath,
                    "./cmd/skirk",
                )
                environment("GOOS", "android")
                environment("GOARCH", goArch)
                environment("CGO_ENABLED", "0")
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
            APP_ABI := arm64-v8a
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

        val outputDir = generatedHevJniLibs.get().dir("arm64-v8a").asFile
        outputDir.mkdirs()
        hevSourceDir.file("libs/arm64-v8a/libhev-socks5-tunnel.so").asFile
            .copyTo(outputDir.resolve("libhev-socks5-tunnel.so"), overwrite = true)
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
        versionCode = 33
        versionName = skirkAppVersion

        ndk {
            abiFilters += "arm64-v8a"
        }
    }

    buildFeatures {
        compose = true
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
