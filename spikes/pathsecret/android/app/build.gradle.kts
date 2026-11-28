plugins {
  id("com.android.application")
}

android {
  namespace = "dev.spike.pathsecret"
  compileSdk { version = release(36) }

  defaultConfig {
    applicationId = "dev.spike.pathsecret"
    minSdk = 26
    targetSdk = 36
    versionCode = 1
    versionName = "0.0.1-spike"
  }

  compileOptions {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
  }
}

dependencies {
  implementation("androidx.media3:media3-exoplayer:1.11.1")
  implementation("androidx.media3:media3-exoplayer-hls:1.11.1")
}
