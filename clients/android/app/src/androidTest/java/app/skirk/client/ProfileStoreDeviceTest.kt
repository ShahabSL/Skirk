package app.skirk.client

import android.content.Context
import android.content.ContextWrapper
import android.content.SharedPreferences
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.io.File
import java.nio.file.Files
import java.util.UUID
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class ProfileStoreDeviceTest {
    @Test
    fun clearAllCollectsLegacyCredentialsWithMissingOrCorruptProfileRecords() = withStore { store, prefs, configs ->
        val orphan = File(configs, "removed.skirk")
        for (records in listOf(null, "{invalid")) {
            prefs.edit().putString("profiles", records).putString("selectedProfileId", "removed").commit()
            orphan.writeText("synthetic fixture")
            store.deleteAll()
            assertFalse(orphan.exists())
            assertTrue(prefs.all.isEmpty())
        }
    }

    @Test
    fun removingTheLastSavedProfileCollectsOrphansButAStaleRemovalDoesNot() = withStore { store, _, configs ->
        val orphan = File(configs, "orphan.json").apply { writeText("{}") }
        store.deleteProfile("missing")
        assertTrue(orphan.exists())

        for (id in listOf("first", "second")) {
            val data = JSONObject().put("id", id).put("name", id).put("rawConfig", "synthetic fixture")
            store.saveProfile(ClientProfile.fromJson(data))
            File(configs, "$id.skirk").writeText("synthetic fixture")
        }
        val first = File(configs, "first.skirk")
        val second = File(configs, "second.skirk")
        store.deleteProfile("missing")
        assertTrue(first.exists() && second.exists() && orphan.exists())
        store.deleteProfile("first")
        assertFalse(first.exists())
        assertTrue(second.exists() && orphan.exists())
        assertEquals("second", store.selectedProfileId())
        store.deleteProfile("second")
        assertFalse(second.exists() || orphan.exists())
        assertTrue(store.listProfiles().isEmpty())
    }

    private fun withStore(test: (ProfileStore, SharedPreferences, File) -> Unit) {
        val app = InstrumentationRegistry.getInstrumentation().targetContext
        val prefName = "profile-cleanup-test-${UUID.randomUUID()}"
        val privateDir = Files.createTempDirectory(app.cacheDir.toPath(), "profile-store-test").toFile()
        val prefs = app.getSharedPreferences(prefName, Context.MODE_PRIVATE)
        val context = object : ContextWrapper(app) {
            override fun getFilesDir(): File = privateDir
            override fun getSharedPreferences(name: String, mode: Int) = prefs
        }
        try {
            val configs = File(privateDir, "configs").apply { mkdir() }
            test(ProfileStore(context), prefs, configs)
        } finally {
            app.deleteSharedPreferences(prefName)
            privateDir.deleteRecursively()
        }
    }
}
