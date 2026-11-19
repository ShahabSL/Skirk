package app.skirk.client

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.FolderOpen
import androidx.compose.material.icons.outlined.Visibility
import androidx.compose.material.icons.outlined.VisibilityOff
import androidx.compose.material.icons.rounded.Add
import androidx.compose.material.icons.rounded.ContentPaste
import androidx.compose.material.icons.rounded.MoreVert
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.FilledTonalButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.unit.dp

internal fun displayProfileNames(profiles: List<ClientProfile>): Map<String, String> {
    val used = profiles.map { it.name.lowercase() }.toMutableSet()
    return buildMap {
        profiles.groupBy { it.name.lowercase() }.values.forEach { sameName ->
            if (sameName.size == 1) {
                val profile = sameName.first()
                put(profile.id, profile.name)
            } else {
                var suffix = 1
                sameName.forEach { profile ->
                    var label: String
                    do { label = "${profile.name} · ${suffix++}" } while (!used.add(label.lowercase()))
                    put(profile.id, label)
                }
            }
        }
    }
}

internal fun nextProfileName(profiles: List<ClientProfile>): String {
    val names = profiles.map { it.name.lowercase() }.toSet()
    val base = "Skirk profile"
    if (base.lowercase() !in names) return base
    return generateSequence(2) { it + 1 }.map { "$base $it" }.first { it.lowercase() !in names }
}

@Composable
internal fun ProfilesPage(
    profiles: List<ClientProfile>,
    names: Map<String, String>,
    currentId: String?,
    running: Boolean,
    onSelect: (ClientProfile) -> Unit,
    onAdd: () -> Unit,
    onReplace: (ClientProfile) -> Unit,
    onRename: (ClientProfile) -> Unit,
    onRemove: (ClientProfile) -> Unit,
) {
    LazyColumn(
        modifier = Modifier.fillMaxSize(),
        contentPadding = PaddingValues(start = 24.dp, end = 24.dp, top = 12.dp, bottom = 24.dp),
    ) {
        item {
            if (running) {
                Text("Disconnect to switch or edit profiles.", style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(bottom = 8.dp))
            } else {
                FilledTonalButton(onClick = onAdd, modifier = Modifier.heightIn(min = 48.dp)) {
                    Icon(Icons.Rounded.Add, contentDescription = null, modifier = Modifier.size(20.dp))
                    Spacer(Modifier.width(8.dp))
                    Text("Add profile")
                }
            }
            if (profiles.isEmpty()) {
                Text("No saved profiles.", modifier = Modifier.padding(top = 12.dp),
                    style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            Spacer(Modifier.height(12.dp))
        }
        items(profiles, key = { it.id }) { profile ->
            HorizontalDivider()
            ProfileListRow(
                name = names.getValue(profile.id),
                mode = modeLabel(profile.connectionMode),
                selected = currentId == profile.id,
                running = running,
                onSelect = { onSelect(profile) },
                onReplace = { onReplace(profile) },
                onRename = { onRename(profile) },
                onRemove = { onRemove(profile) },
            )
        }
        if (profiles.isNotEmpty()) item { HorizontalDivider() }
    }
}

@Composable
private fun ProfileListRow(
    name: String,
    mode: String,
    selected: Boolean,
    running: Boolean,
    onSelect: () -> Unit,
    onReplace: () -> Unit,
    onRename: () -> Unit,
    onRemove: () -> Unit,
) {
    var menuOpen by remember { mutableStateOf(false) }
    Row(Modifier.fillMaxWidth().heightIn(min = 68.dp), verticalAlignment = Alignment.CenterVertically) {
        Row(
            Modifier.weight(1f)
                .heightIn(min = 68.dp)
                .selectable(selected, enabled = !running, role = Role.RadioButton, onClick = onSelect)
                .padding(top = 12.dp, bottom = 12.dp, end = 8.dp),
            horizontalArrangement = Arrangement.spacedBy(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            RadioButton(selected = selected, onClick = null, enabled = !running)
            Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
                Text(name, style = MaterialTheme.typography.titleMedium)
                Text(
                    if (selected) "${if (running) "In use" else "Selected"} · $mode" else mode,
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        if (!running) {
            Box {
                IconButton(onClick = { menuOpen = true }) {
                    Icon(Icons.Rounded.MoreVert, contentDescription = "Options for $name")
                }
                DropdownMenu(expanded = menuOpen, onDismissRequest = { menuOpen = false }) {
                    DropdownMenuItem(text = { Text("Rename") }, onClick = { menuOpen = false; onRename() })
                    DropdownMenuItem(text = { Text("Replace profile") }, onClick = { menuOpen = false; onReplace() })
                    DropdownMenuItem(
                        text = { Text("Remove", color = MaterialTheme.colorScheme.error) },
                        onClick = { menuOpen = false; onRemove() },
                    )
                }
            }
        }
    }
}

@Composable
internal fun ProfileForm(
    replacingName: String?,
    profileName: String,
    rawConfig: String,
    error: String,
    reading: Boolean,
    enabled: Boolean,
    showHeading: Boolean,
    onNameChange: (String) -> Unit,
    onTextChange: (String) -> Unit,
    onPaste: () -> Unit,
    onChooseFile: () -> Unit,
    onCancelRead: () -> Unit,
    onSave: () -> Unit,
) {
    var showText by remember { mutableStateOf(false) }
    val focus = LocalFocusManager.current
    val hasText = rawConfig.isNotBlank()
    Column(
        Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 24.dp, vertical = 20.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        if (showHeading) {
            Text("Add a profile", style = MaterialTheme.typography.headlineLarge,
                modifier = Modifier.semantics { heading() })
        }
        Text(
            when {
                !enabled -> "Disconnect before changing profiles."
                replacingName != null -> "Paste the renewed profile for $replacingName. Your connection settings stay the same."
                else -> "Copy a profile from your Skirk exit and paste it here."
            },
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            style = MaterialTheme.typography.bodyLarge,
        )
        Spacer(Modifier.height(4.dp))
        OutlinedTextField(
            value = rawConfig,
            onValueChange = onTextChange,
            enabled = enabled,
            modifier = Modifier.fillMaxWidth(),
            label = { Text(if (replacingName == null) "Profile text" else "Renewed profile text") },
            minLines = 2,
            maxLines = 4,
            isError = error.isNotBlank(),
            supportingText = if (error.isNotBlank()) ({
                Text(error, modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite })
            }) else null,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password, autoCorrectEnabled = false),
            visualTransformation = if (showText) VisualTransformation.None else PasswordVisualTransformation(),
            trailingIcon = if (hasText) ({
                IconButton(onClick = { showText = !showText }, enabled = enabled) {
                    Icon(
                        if (showText) Icons.Outlined.VisibilityOff else Icons.Outlined.Visibility,
                        contentDescription = if (showText) "Hide profile text" else "Show profile text",
                    )
                }
            }) else null,
        )
        if (hasText && replacingName == null) {
            OutlinedTextField(
                value = profileName,
                onValueChange = { onNameChange(it.take(MAX_NAME_LENGTH)) },
                enabled = enabled,
                modifier = Modifier.fillMaxWidth(),
                label = { Text("Name (optional)") },
                singleLine = true,
            )
        }
        if (reading) {
            LinearProgressIndicator(Modifier.fillMaxWidth())
            Text("Reading profile file…", modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite },
                style = MaterialTheme.typography.bodyMedium)
            TextButton(onClick = onCancelRead) { Text("Cancel file import") }
        }
        Button(
            onClick = {
                focus.clearFocus()
                if (hasText) onSave() else { showText = false; onPaste() }
            },
            enabled = enabled && !reading,
            modifier = Modifier.fillMaxWidth().heightIn(min = 56.dp),
        ) {
            if (!hasText) {
                Icon(Icons.Rounded.ContentPaste, contentDescription = null, modifier = Modifier.size(20.dp))
                Spacer(Modifier.width(8.dp))
            }
            Text(when {
                !hasText -> "Paste profile"
                replacingName != null -> "Replace profile"
                else -> "Save profile"
            })
        }
        Column {
            if (hasText) {
                TextButton(onClick = { focus.clearFocus(); showText = false; onPaste() }, enabled = enabled && !reading) {
                    Icon(Icons.Rounded.ContentPaste, contentDescription = null, modifier = Modifier.size(18.dp))
                    Spacer(Modifier.width(8.dp))
                    Text("Paste again")
                }
            }
            TextButton(onClick = { focus.clearFocus(); showText = false; onChooseFile() }, enabled = enabled && !reading) {
                Icon(Icons.Outlined.FolderOpen, contentDescription = null, modifier = Modifier.size(18.dp))
                Spacer(Modifier.width(8.dp))
                Text("Choose profile file")
            }
        }
    }
}

@Composable
internal fun RenameProfileDialog(name: String, onDismiss: () -> Unit, onSave: (String) -> Unit) {
    var text by rememberSaveable(name) { mutableStateOf(name) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Rename profile") },
        text = {
            OutlinedTextField(
                value = text,
                onValueChange = { text = it.take(MAX_NAME_LENGTH) },
                label = { Text("Profile name") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
        },
        confirmButton = { TextButton(onClick = { onSave(text.trim()) }, enabled = text.isNotBlank()) { Text("Save") } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

private const val MAX_NAME_LENGTH = 80
