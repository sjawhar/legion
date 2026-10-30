import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

// The Dispatch server the app opens. This repository names no deployment, so a build takes it
// from `-PdispatchUrl=https://...` or from `dispatch.url` in local.properties. Without either the
// app asks for it on first launch.
val localProperties = Properties().apply {
    val file = rootProject.file("local.properties")
    if (file.exists()) file.inputStream().use { load(it) }
}
val dispatchUrl: String =
    (findProperty("dispatchUrl") as String?) ?: localProperties.getProperty("dispatch.url") ?: ""

android {
    namespace = "dev.legion.dispatch"
    compileSdk = 35

    defaultConfig {
        applicationId = "dev.legion.dispatch"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
        buildConfigField("String", "DISPATCH_URL", "\"${dispatchUrl.trimEnd('/')}\"")
    }

    buildFeatures {
        buildConfig = true
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            // Signed with the debug key so a sideloaded release build installs; a store release
            // would add a signing config of its own.
            signingConfig = signingConfigs.getByName("debug")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.15.0")
    implementation("androidx.activity:activity-ktx:1.9.3")
    implementation("androidx.work:work-runtime-ktx:2.10.0")
    implementation("com.squareup.okhttp3:okhttp:4.12.0")

    testImplementation("junit:junit:4.13.2")
    // Android's org.json is a stub on the JVM; the unit tests parse with the real one.
    testImplementation("org.json:json:20240303")
}
