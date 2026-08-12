package com.abyssinialabs.latemexample

import android.app.Activity
import android.graphics.Color
import android.os.Build
import android.os.Bundle
import android.view.Gravity
import android.widget.LinearLayout
import android.widget.TextView

/**
 * The app deliberately does almost nothing, and uses no AndroidX. The point
 * under test is the pipeline — Gradle, signing, bundling, upload — not this
 * screen, and every dependency added here is another thing that can fail for a
 * reason unrelated to the runner.
 *
 * It shows the versionCode, because that is the one thing worth seeing on a
 * device: it says *which* build reached Play, which is the question you actually
 * have when a run finishes.
 */
class MainActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val info = packageManager.getPackageInfo(packageName, 0)
        @Suppress("DEPRECATION")
        val code = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P)
            info.longVersionCode else info.versionCode.toLong()

        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            gravity = Gravity.CENTER
            setBackgroundColor(Color.WHITE)
        }
        root.addView(TextView(this).apply {
            text = "Latem"
            textSize = 34f
            gravity = Gravity.CENTER
        })
        root.addView(TextView(this).apply {
            text = "build ${info.versionName} ($code)"
            textSize = 14f
            gravity = Gravity.CENTER
            setTextColor(Color.DKGRAY)
        })
        setContentView(root)
    }
}
