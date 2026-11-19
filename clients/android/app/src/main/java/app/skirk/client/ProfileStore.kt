package app.skirk.client

import android.content.Context
import org.json.JSONArray

class ProfileStore(context: Context) {
    private val prefs = context.getSharedPreferences("skirk_profiles", Context.MODE_PRIVATE)
    private val filesDir = context.filesDir

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

    fun saveProfile(profile: ClientProfile, makeSelected: Boolean = false) {
        val current = listProfiles()
        val previous = current.firstOrNull { it.id == profile.id }
        val next = if (previous != null) {
            current.map { if (it.id == profile.id) profile else it }
        } else {
            current + profile
        }
        val selected = if (makeSelected) profile.id else {
            selectedProfileId()?.takeIf { id -> next.any { it.id == id } } ?: next.firstOrNull()?.id
        }
        val array = JSONArray()
        next.forEach { array.put(it.toJson()) }
        prefs.edit()
            .putString(KEY_PROFILES, array.toString())
            .putString(KEY_SELECTED, selected)
            .apply()
        if (previous != null && previous.rawConfig != profile.rawConfig) {
            deleteLegacyRuntimeProfiles(filesDir, profile.id)
        }
    }

    fun selectProfile(profileId: String?) {
        prefs.edit().putString(KEY_SELECTED, profileId).apply()
    }

    fun deleteProfile(profileId: String) {
        val current = listProfiles()
        val next = current.filterNot { it.id == profileId }
        val nextSelected = if (selectedProfileId() == profileId) next.firstOrNull()?.id else selectedProfileId()
        val array = JSONArray()
        next.forEach { array.put(it.toJson()) }
        prefs.edit()
            .putString(KEY_PROFILES, array.toString())
            .putString(KEY_SELECTED, nextSelected)
            .apply()
        if (current.isNotEmpty() && next.isEmpty()) {
            deleteAllLegacyRuntimeProfiles(filesDir)
        } else {
            deleteLegacyRuntimeProfiles(filesDir, profileId)
        }
    }

    fun deleteAll() {
        prefs.edit().clear().apply()
        deleteAllLegacyRuntimeProfiles(filesDir)
    }

    private companion object {
        const val KEY_PROFILES = "profiles"
        const val KEY_SELECTED = "selectedProfileId"
    }
}
