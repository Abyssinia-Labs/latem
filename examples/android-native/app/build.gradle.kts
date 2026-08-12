plugins {
  id("com.android.application")
}

android {
  namespace = "com.abyssinialabs.latemexample"
  compileSdk = 36

  defaultConfig {
    applicationId = "com.abyssinialabs.latemexample"
    minSdk = 26
    targetSdk = 36

    // Overridden on the command line by ci/android/build.sh. Play rejects a
    // versionCode it has already accepted, so a pinned value uploads exactly
    // once — the same trap as CFBundleVersion on iOS (amendment A36).
    versionCode = (project.findProperty("latemVersionCode") as String?)?.toInt() ?: 1
    versionName = "1.0"
  }

  signingConfigs {
    create("release") {
      // Populated from the environment by ci/android/build.sh. Absent here so
      // the file is safe to commit and the debug build needs nothing.
      val store = System.getenv("ANDROID_KEYSTORE_PATH")
      if (store != null) {
        storeFile = file(store)
        storePassword = System.getenv("ANDROID_KEYSTORE_PASSWORD")
        keyAlias = System.getenv("ANDROID_KEY_ALIAS")
        keyPassword = System.getenv("ANDROID_KEY_PASSWORD")
      }
    }
  }

  buildTypes {
    release {
      isMinifyEnabled = false
      // Only attach the signing config when a keystore was actually supplied,
      // so `assembleRelease` still works unsigned for a smoke test.
      signingConfig = if (System.getenv("ANDROID_KEYSTORE_PATH") != null)
        signingConfigs.getByName("release") else null
    }
  }

  compileOptions {
    sourceCompatibility = JavaVersion.VERSION_17
    targetCompatibility = JavaVersion.VERSION_17
  }
}
