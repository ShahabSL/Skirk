plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}

val repoRoot = layout.projectDirectory.dir("../../..")
val generatedJniLibs = layout.buildDirectory.dir("generated/skirk-go/jniLibs")

val buildSkirkAndroidSidecar = tasks.register("buildSkirkAndroidSidecar") {
    group = "build"
    description = "Build the Skirk Go engine as Android native executables packaged as JNI libs."
    inputs.dir(repoRoot.dir("cmd"))
    inputs.dir(repoRoot.dir("internal"))
    inputs.file(repoRoot.file("go.mod"))
    outputs.dir(generatedJniLibs)

    doLast {
        val targets = listOf(
            Triple("arm64-v8a", "arm64", "libskirk.so"),
        )
        targets.forEach { (abi, goArch, fileName) ->
            val outputDir = generatedJniLibs.get().dir(abi).asFile
            outputDir.mkdirs()
            exec {
                workingDir = repoRoot.asFile
                executable = "go"
                args(
                    "build",
                    "-trimpath",
                    "-buildmode=pie",
                    "-ldflags",
                    "-s -w -X main.version=android-debug",
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

android {
    namespace = "app.skirk.client"
    compileSdk = 35

    defaultConfig {
        applicationId = "app.skirk.client"
        minSdk = 26
        targetSdk = 35
        versionCode = 5
        versionName = "0.1.4"
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
            jniLibs.srcDir(generatedJniLibs)
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
}

dependencies {
    val composeBom = platform("androidx.compose:compose-bom:2024.12.01")
    implementation(composeBom)
    implementation("androidx.activity:activity-compose:1.9.3")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.ui:ui-tooling-preview")
    debugImplementation("androidx.compose.ui:ui-tooling")
}
