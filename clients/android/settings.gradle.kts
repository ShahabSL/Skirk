pluginManagement {
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
    }
}

dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google()
        mavenCentral()
    }
}

rootProject.name = "SkirkAndroid"
include(":app")

val sdkPath = providers.gradleProperty("android.sdk.path").orNull
    ?: System.getenv("ANDROID_SDK_ROOT")
    ?: System.getenv("ANDROID_HOME")
if (!sdkPath.isNullOrBlank()) {
    val localPropertiesFile = rootDir.resolve("local.properties")
    if (!localPropertiesFile.exists()) {
        localPropertiesFile.writeText("""
            sdk.dir=$sdkPath
        """.trimIndent() + "\n")
    }
}
