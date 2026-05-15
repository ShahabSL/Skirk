package app.skirk.client

import android.content.Context
import org.json.JSONArray

class ProfileStore(context: Context) {
    private val prefs = context.getSharedPreferences("skirk_profiles", Context.MODE_PRIVATE)

    fun listProfiles(): List<ClientProfile> {
        val raw = prefs.getString(KEY_PROFILES, "[]") ?: "[]"
        val array = runCatching { JSONArray(raw) }.getOrElse { JSONArray() }
        return buildList {
            for (index in 0 until array.length()) {
                runCatching {
                    add(ClientProfile.fromJson(array.getJSONObject(index)))
                }
            }
        }
    }

    fun selectedProfileId(): String? = prefs.getString(KEY_SELECTED, null)

    fun selectedProfile(): ClientProfile? {
        val profiles = listProfiles()
        val selected = selectedProfileId()
        return profiles.firstOrNull { it.id == selected } ?: profiles.firstOrNull()
    }

    fun saveProfile(profile: ClientProfile) {
        val next = listProfiles().filterNot { it.id == profile.id } + profile
        writeProfiles(next)
        prefs.edit().putString(KEY_SELECTED, profile.id).apply()
    }

    fun selectProfile(profileId: String?) {
        prefs.edit().putString(KEY_SELECTED, profileId).apply()
    }

    fun deleteProfile(profileId: String) {
        val next = listProfiles().filterNot { it.id == profileId }
        writeProfiles(next)
        val nextSelected = if (selectedProfileId() == profileId) next.firstOrNull()?.id else selectedProfileId()
        prefs.edit().putString(KEY_SELECTED, nextSelected).apply()
    }

    fun deleteAll() {
        prefs.edit().clear().apply()
    }

    private fun writeProfiles(profiles: List<ClientProfile>) {
        val array = JSONArray()
        profiles.forEach { array.put(it.toJson()) }
        prefs.edit().putString(KEY_PROFILES, array.toString()).apply()
    }

    private companion object {
        const val KEY_PROFILES = "profiles"
        const val KEY_SELECTED = "selectedProfileId"
    }
}
