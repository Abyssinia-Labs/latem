// Versions are pinned, not ranges. A CI runner that silently picks up a new
// Android Gradle Plugin between two runs of the same commit is not reproducible,
// which is the property §12's fresh clone exists to provide.
// AGP 9.0+ has built-in Kotlin support; applying org.jetbrains.kotlin.android
// alongside it is a hard error ("no longer required for Kotlin support since
// AGP 9.0"). Nothing here declares a Kotlin plugin version any more — AGP owns
// it, which is one fewer version pair to keep compatible.
plugins {
  id("com.android.application") version "9.3.1" apply false
}
