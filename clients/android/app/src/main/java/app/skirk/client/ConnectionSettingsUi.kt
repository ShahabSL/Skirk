package app.skirk.client

import android.content.Context
import android.content.Intent
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.ime
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Add
import androidx.compose.material.icons.rounded.Apps
import androidx.compose.material.icons.rounded.ChevronRight
import androidx.compose.material.icons.rounded.CloudQueue
import androidx.compose.material.icons.rounded.ContentCopy
import androidx.compose.material.icons.rounded.PowerSettingsNew
import androidx.compose.material.icons.rounded.Refresh
import androidx.compose.material.icons.rounded.Remove
import androidx.compose.material.icons.rounded.Speed
import androidx.compose.material.icons.rounded.Storage
import androidx.compose.material.icons.rounded.WifiTethering
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.produceState
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.scale
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.platform.LocalLayoutDirection
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.LayoutDirection
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.text.NumberFormat

private enum class SettingsDialog { Mode, Route, Ports }

@Composable
internal fun SettingsPage(
    profile: ClientProfile,
    running: Boolean,
    onModeChange: (String) -> Unit,
    onRouteModeChange: (String) -> Unit,
    onProxyShareLanChange: (Boolean) -> Unit,
    onProxyPortsChange: (Int, Int) -> Unit,
    onOpenVpnApps: () -> Unit,
    onOpenPerformance: () -> Unit,
    onOpenLogs: () -> Unit,
    modifier: Modifier = Modifier,
) {
    var dialog by rememberSaveable(profile.id) { mutableStateOf<SettingsDialog?>(null) }
    var showLicenses by rememberSaveable { mutableStateOf(false) }
    val proxy = profile.connectionMode == ClientProfile.CONNECTION_MODE_PROXY
    val editable = !running

    Column(
        modifier = modifier.fillMaxSize().verticalScroll(rememberScrollState())
            .padding(start = 24.dp, end = 24.dp, bottom = 24.dp),
    ) {
        SectionTitle("Connection")
        if (running) {
            SupportingText("Disconnect to edit this profile’s connection settings.", Modifier.padding(bottom = 8.dp))
        }
        SettingsRow(
            title = "Connection mode",
            detail = modeLabel(profile.connectionMode),
            icon = Icons.Rounded.PowerSettingsNew,
            enabled = editable,
            onClick = { dialog = SettingsDialog.Mode },
        )
        SettingsRow(
            title = "Google access route",
            detail = googleRouteLabel(profile.routeMode),
            icon = Icons.Rounded.CloudQueue,
            enabled = editable,
            onClick = { dialog = SettingsDialog.Route },
        )
        if (proxy) {
            SettingsRow(
                title = "Proxy ports",
                detail = proxyEndpoints(profile, profile.shareLan).replace(" · HTTP ", "\nHTTP "),
                icon = Icons.Rounded.WifiTethering,
                enabled = editable,
                onClick = { dialog = SettingsDialog.Ports },
            )
            SettingSwitch(
                title = "Share on LAN",
                detail = if (profile.shareLan) "Other devices on this network can use the proxy. Keep this on only on a trusted network."
                    else "Only this phone can use the proxy.",
                checked = profile.shareLan,
                enabled = editable,
                onCheckedChange = onProxyShareLanChange,
            )
        } else {
            SettingsRow(
                title = "VPN apps",
                detail = vpnAppsSummary(profile),
                icon = Icons.Rounded.Apps,
                enabled = editable,
                onClick = onOpenVpnApps,
            )
        }
        HorizontalDivider(Modifier.padding(top = 8.dp, bottom = 4.dp))
        SectionTitle("Tools")
        SettingsRow(
            title = "Performance",
            detail = performancePresetLabel(profile.performance.preset),
            icon = Icons.Rounded.Speed,
            onClick = onOpenPerformance,
        )
        if (!proxy) {
            SettingsRow(
                title = "Local ports",
                detail = "Change the VPN’s local SOCKS5 port",
                icon = Icons.Rounded.WifiTethering,
                enabled = editable,
                onClick = { dialog = SettingsDialog.Ports },
            )
        }
        SettingsRow(
            title = "Connection logs",
            detail = "Troubleshoot connection issues",
            icon = Icons.Rounded.Storage,
            onClick = onOpenLogs,
        )
        SupportingText("Skirk ${BuildConfig.VERSION_NAME}", Modifier.padding(top = 20.dp))
        TextButton(onClick = { showLicenses = true }) { Text("Open-source licenses") }
    }

    if (showLicenses) {
        val assets = LocalContext.current.assets
        val paragraphs = remember(assets) {
            listOf("licenses/mobile.txt", "licenses/android.txt").flatMap { path ->
                assets.open(path).bufferedReader().use { it.readText() }.trim().split("\n\n")
            }
        }
        AlertDialog(
            onDismissRequest = { showLicenses = false },
            title = { Text("Open-source licenses") },
            text = {
                LazyColumn(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    items(paragraphs) { paragraph ->
                        Text(paragraph, style = MaterialTheme.typography.bodySmall)
                    }
                }
            },
            confirmButton = { TextButton(onClick = { showLicenses = false }) { Text("Close") } },
        )
    }

    val dismiss = { dialog = null }
    if (editable) when (dialog) {
        SettingsDialog.Mode -> SettingsChoiceDialog(
            title = "Connection mode",
            selected = profile.connectionMode,
            options = listOf(
                SettingChoice(ClientProfile.CONNECTION_MODE_VPN, "VPN", "Route all apps or only the apps you choose."),
                SettingChoice(ClientProfile.CONNECTION_MODE_PROXY, "Proxy", "Use SOCKS5 or HTTP from apps that support a proxy."),
            ),
            onDismiss = dismiss,
            onSelect = onModeChange,
        )
        SettingsDialog.Route -> SettingsChoiceDialog(
            title = "Google access route",
            selected = profile.routeMode,
            options = listOf(
                SettingChoice(ClientProfile.ROUTE_GOOGLE_FRONT, "Google front", "Default. Reaches Drive through a Google front."),
                SettingChoice(ClientProfile.ROUTE_DIRECT, "Direct Google", "Use when your network can reach the Drive API directly."),
            ),
            onDismiss = dismiss,
            onSelect = onRouteModeChange,
        )
        SettingsDialog.Ports -> LocalPortsDialog(profile, proxy, dismiss) { socks, http ->
            onProxyPortsChange(socks, http)
            dismiss()
        }
        null -> Unit
    }
}

@Composable
internal fun SettingsRow(
    title: String,
    detail: String? = null,
    icon: ImageVector? = null,
    enabled: Boolean = true,
    modifier: Modifier = Modifier,
    maxTitleLines: Int = Int.MAX_VALUE,
    maxDetailLines: Int = Int.MAX_VALUE,
    onClick: () -> Unit,
) {
    val colors = MaterialTheme.colorScheme
    val rtl = LocalLayoutDirection.current == LayoutDirection.Rtl
    Row(
        modifier = modifier.fillMaxWidth().heightIn(min = 48.dp)
            .clickable(enabled = enabled, role = Role.Button, onClick = onClick)
            .padding(vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        icon?.let {
            Icon(it, contentDescription = null, tint = colors.onSurfaceVariant, modifier = Modifier.size(24.dp))
        }
        RowText(title, detail, Modifier.weight(1f), maxTitleLines, maxDetailLines)
        if (enabled) {
            Icon(
                Icons.Rounded.ChevronRight,
                contentDescription = null,
                tint = colors.onSurfaceVariant,
                modifier = Modifier.size(22.dp).scale(if (rtl) -1f else 1f, 1f),
            )
        }
    }
}

@Composable
private fun SectionTitle(title: String, modifier: Modifier = Modifier) {
    Text(
        title,
        modifier = modifier.padding(top = 16.dp, bottom = 4.dp).semantics { heading() },
        color = MaterialTheme.colorScheme.primary,
        style = MaterialTheme.typography.titleSmall,
    )
}

@Composable
private fun SupportingText(text: String, modifier: Modifier = Modifier) {
    Text(text, modifier, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
}

@Composable
private fun RowText(
    title: String,
    detail: String?,
    modifier: Modifier = Modifier,
    maxTitleLines: Int = Int.MAX_VALUE,
    maxDetailLines: Int = Int.MAX_VALUE,
) {
    Column(modifier, verticalArrangement = Arrangement.spacedBy(2.dp)) {
        Text(title, style = MaterialTheme.typography.bodyLarge, maxLines = maxTitleLines, overflow = TextOverflow.Ellipsis)
        if (detail != null) Text(detail, style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = maxDetailLines, overflow = TextOverflow.Ellipsis)
    }
}

private data class SettingChoice(val value: String, val label: String, val detail: String? = null)

@Composable
private fun SettingsChoiceDialog(
    title: String,
    selected: String,
    options: List<SettingChoice>,
    onDismiss: () -> Unit,
    onSelect: (String) -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()).selectableGroup()) {
                options.forEach { choice ->
                    SettingRadio(choice.label, choice.detail, selected == choice.value) {
                        onSelect(choice.value)
                        onDismiss()
                    }
                }
            }
        },
        confirmButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun SettingRadio(
    title: String,
    detail: String?,
    selected: Boolean,
    enabled: Boolean = true,
    onClick: () -> Unit,
) {
    Row(
        modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp)
            .selectable(selected = selected, enabled = enabled, role = Role.RadioButton, onClick = onClick)
            .padding(horizontal = 4.dp, vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        RadioButton(selected = selected, onClick = null, enabled = enabled)
        RowText(title, detail, Modifier.weight(1f))
    }
}

@Composable
private fun SettingSwitch(
    title: String,
    detail: String,
    checked: Boolean,
    enabled: Boolean,
    onCheckedChange: (Boolean) -> Unit,
) {
    Row(
        Modifier.fillMaxWidth().heightIn(min = 48.dp)
            .toggleable(checked, enabled = enabled, role = Role.Switch, onValueChange = onCheckedChange)
            .padding(vertical = 12.dp),
        horizontalArrangement = Arrangement.spacedBy(16.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        RowText(title, detail, Modifier.weight(1f))
        Switch(checked = checked, onCheckedChange = null, enabled = enabled)
    }
}

@Composable
private fun LocalPortsDialog(profile: ClientProfile, proxy: Boolean, onDismiss: () -> Unit, onSave: (Int, Int) -> Unit) {
    var socks by rememberSaveable { mutableStateOf(profile.socksPort.toString()) }
    var http by rememberSaveable { mutableStateOf(profile.httpPort.toString()) }
    var submitted by rememberSaveable { mutableStateOf(false) }
    val socksResult = runCatching { ClientProfile.validateSocksPort(socks.toIntOrNull() ?: 0) }
    val httpResult = runCatching { ClientProfile.validateHttpPort(http.toIntOrNull() ?: 0, socksResult.getOrDefault(0)) }
    val socksError = if (submitted) socksResult.exceptionOrNull()?.message else null
    val httpError = if (submitted) httpResult.exceptionOrNull()?.message else null

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(if (proxy) "Proxy ports" else "Local ports") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                if (!proxy) SupportingText("The VPN uses SOCKS5 internally. The HTTP port applies only in Proxy mode.")
                PortField("SOCKS5 port", socks, socksError) { socks = it }
                PortField("HTTP port", http, httpError) { http = it }
            }
        },
        confirmButton = {
            TextButton(onClick = {
                submitted = true
                if (socksResult.isSuccess && httpResult.isSuccess) onSave(socksResult.getOrThrow(), httpResult.getOrThrow())
            }) { Text("Save") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun PortField(label: String, value: String, error: String?, onChange: (String) -> Unit) {
    OutlinedTextField(
        value = value,
        onValueChange = { onChange(it.filter { char -> char in '0'..'9' }.take(5)) },
        label = { Text(label) },
        singleLine = true,
        isError = error != null,
        supportingText = if (error == null) null else ({
            Text(error, Modifier.semantics { liveRegion = LiveRegionMode.Polite })
        }),
        modifier = Modifier.fillMaxWidth(),
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
    )
}

private data class VpnAppEntry(val packageName: String, val label: String)

@Suppress("DEPRECATION")
private fun queryVpnApps(context: Context): List<VpnAppEntry> {
    val manager = context.packageManager
    val launcher = Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER)
    return manager.queryIntentActivities(launcher, 0)
        .mapNotNull { activity ->
            val name = activity.activityInfo?.packageName ?: return@mapNotNull null
            if (name == context.packageName) return@mapNotNull null
            VpnAppEntry(name, activity.loadLabel(manager).toString().ifBlank { name })
        }
        .distinctBy { it.packageName }
        .sortedWith(compareBy<VpnAppEntry> { it.label.lowercase() }.thenBy { it.packageName })
}

@Composable
internal fun VpnAppsPage(
    profile: ClientProfile,
    onSave: (String, List<String>, List<String>) -> Unit,
    modifier: Modifier = Modifier,
    enabled: Boolean = true,
) {
    val context = LocalContext.current.applicationContext
    val focusManager = LocalFocusManager.current
    val keyboardVisible = WindowInsets.ime.getBottom(LocalDensity.current) > 0
    val installed by produceState<List<VpnAppEntry>?>(null, context) {
        value = withContext(Dispatchers.IO) { queryVpnApps(context) }
    }
    var mode by rememberSaveable(profile.id) { mutableStateOf(profile.vpnAppMode) }
    var allowedPackages by rememberSaveable(profile.id) {
        mutableStateOf(ArrayList(profile.vpnAllowedApps.filterNot { it == context.packageName }))
    }
    var excludedPackages by rememberSaveable(profile.id) {
        mutableStateOf(ArrayList(profile.vpnExcludedApps.filterNot { it == context.packageName }))
    }
    val selectedOnly = mode == ClientProfile.VPN_APPS_SELECTED
    val selectedPackages = if (selectedOnly) allowedPackages else excludedPackages
    var search by rememberSaveable(profile.id) { mutableStateOf("") }
    val installedNames = remember(installed) { installed.orEmpty().map { it.packageName }.toSet() }
    val duplicatedLabels = remember(installed) { installed.orEmpty().groupingBy { it.label }.eachCount() }
    val selectedCount = selectedPackages.count { it in installedNames }
    val visibleApps = remember(installed, selectedPackages, search) {
        val missing = selectedPackages.filterNot { it in installedNames }.map { VpnAppEntry(it, it) }
        (installed.orEmpty() + missing).filter {
            it.label.contains(search.trim(), ignoreCase = true) || it.packageName.contains(search.trim(), ignoreCase = true)
        }
    }

    BoxWithConstraints(modifier.fillMaxSize()) {
        val showSave = !keyboardVisible || maxHeight >= 280.dp
        Column(Modifier.fillMaxSize()) {
            LazyColumn(Modifier.weight(1f), contentPadding = PaddingValues(start = 24.dp, end = 24.dp, bottom = 12.dp)) {
                item {
                    SectionTitle("Route through Skirk")
                    Column(Modifier.selectableGroup()) {
                        SettingRadio("All apps", "Skirk itself stays outside the VPN.", mode == ClientProfile.VPN_APPS_ALL, enabled) {
                            mode = ClientProfile.VPN_APPS_ALL
                        }
                        SettingRadio("Only selected apps", "Other apps use their usual connection.", selectedOnly, enabled) {
                            mode = ClientProfile.VPN_APPS_SELECTED
                        }
                        SettingRadio("All except selected apps", "Selected apps use their usual connection.",
                            mode == ClientProfile.VPN_APPS_EXCLUDED, enabled) { mode = ClientProfile.VPN_APPS_EXCLUDED }
                    }
                }
                if (mode != ClientProfile.VPN_APPS_ALL) {
                    item {
                        SupportingText(
                            "Android’s “Block connections without VPN” setting can block apps outside the VPN.",
                            Modifier.padding(top = 4.dp, bottom = 12.dp),
                        )
                        OutlinedTextField(
                            value = search,
                            onValueChange = { search = it },
                            label = { Text("Search installed apps") },
                            singleLine = true,
                            modifier = Modifier.fillMaxWidth(),
                            keyboardOptions = KeyboardOptions(imeAction = ImeAction.Search),
                            keyboardActions = KeyboardActions(onSearch = { focusManager.clearFocus() }),
                        )
                        val countLabel = when {
                            installed == null -> "Loading apps…"
                            installedNames.isEmpty() -> "No launchable apps found."
                            selectedCount == 0 -> if (selectedOnly) "Select at least one installed app." else "All apps use Skirk."
                            selectedCount == 1 -> "1 app selected"
                            else -> "$selectedCount apps selected"
                        }
                        SupportingText(countLabel, Modifier.padding(top = 12.dp, bottom = 4.dp).semantics {
                            liveRegion = LiveRegionMode.Polite
                        })
                    }
                    if (installed != null) {
                        if (visibleApps.isEmpty() && search.isNotBlank()) {
                            item { SupportingText("No apps match your search.", Modifier.padding(top = 12.dp)) }
                        }
                        items(visibleApps, key = { it.packageName }) { app ->
                            val checked = app.packageName in selectedPackages
                            val detail = when {
                                app.packageName !in installedNames -> "No longer installed"
                                (duplicatedLabels[app.label] ?: 0) > 1 -> app.packageName
                                else -> null
                            }
                            Row(
                                modifier = Modifier.fillMaxWidth().heightIn(min = 48.dp)
                                    .toggleable(checked, enabled = enabled, role = Role.Checkbox, onValueChange = { include ->
                                        val updated = ArrayList(if (include) selectedPackages + app.packageName
                                            else selectedPackages - app.packageName)
                                        if (selectedOnly) allowedPackages = updated else excludedPackages = updated
                                    }).padding(vertical = 10.dp),
                                verticalAlignment = Alignment.CenterVertically,
                                horizontalArrangement = Arrangement.spacedBy(16.dp),
                            ) {
                                RowText(app.label, detail, Modifier.weight(1f))
                                Checkbox(checked = checked, onCheckedChange = null, enabled = enabled)
                            }
                        }
                    }
                }
            }
            if (showSave) {
                HorizontalDivider()
                Button(
                    onClick = {
                        onSave(mode, allowedPackages.sorted(), excludedPackages.sorted())
                    },
                    enabled = enabled && (!selectedOnly || selectedCount > 0),
                    modifier = Modifier.fillMaxWidth().padding(horizontal = 24.dp, vertical = 8.dp).heightIn(min = 48.dp),
                ) { Text("Save VPN apps") }
            }
        }
    }
}

@Composable
internal fun PerformancePage(
    performance: PerformanceSettings,
    metrics: DriveMetrics?,
    enabled: Boolean,
    onPerformanceChange: (PerformanceSettings) -> Unit,
    modifier: Modifier = Modifier,
) {
    Column(
        modifier = modifier.fillMaxSize().verticalScroll(rememberScrollState())
            .padding(start = 24.dp, end = 24.dp, bottom = 24.dp),
    ) {
        if (!enabled) {
            DriveActivity(metrics, waitingForSample = true)
            HorizontalDivider(Modifier.padding(top = 16.dp, bottom = 4.dp))
        }
        SectionTitle("Performance preset")
        if (!enabled) SupportingText("Disconnect to change performance settings.", Modifier.padding(bottom = 8.dp))
        Column(Modifier.selectableGroup()) {
            val presets = listOf(
                SettingChoice(PerformanceSettings.PRESET_RECOMMENDED, "Recommended", "Standard polling and worker limits."),
                SettingChoice(PerformanceSettings.PRESET_LOWER_USAGE, "Lower usage", "Check Drive less often and use fewer workers."),
                SettingChoice(PerformanceSettings.PRESET_RESPONSIVE, "Responsive", "Poll faster after traffic. Uses more Drive quota."),
                SettingChoice(PerformanceSettings.PRESET_BULK_TRANSFER, "Bulk transfer", "Use more workers for large transfers."),
                SettingChoice(PerformanceSettings.PRESET_CUSTOM, "Custom", "Keep the current values and adjust them below."),
            )
            presets.forEach { preset ->
                SettingRadio(preset.label, preset.detail, performance.preset == preset.value, enabled) {
                    if (performance.preset != preset.value) {
                        onPerformanceChange(if (preset.value == PerformanceSettings.PRESET_CUSTOM)
                            performance.copy(preset = preset.value) else PerformanceSettings.forPreset(preset.value))
                    }
                }
            }
        }
        HorizontalDivider(Modifier.padding(top = 8.dp, bottom = 4.dp))
        if (performance.preset == PerformanceSettings.PRESET_CUSTOM) {
            SectionTitle("Custom limits")
            SupportingText("Shorter intervals and more workers can use Drive quota faster.", Modifier.padding(bottom = 8.dp))
            AdjustPerformanceValue(
                title = "Check interval",
                valueText = "${performance.pollMs} ms",
                value = performance.pollMs,
                step = 250,
                range = PerformanceSettings.CUSTOM_MIN_POLL_MS..60_000,
                enabled = enabled,
                onChange = { onPerformanceChange(performance.copy(pollMs = it)) },
            )
            AdjustPerformanceValue(
                title = "Upload workers",
                valueText = performance.uploadConcurrency.toString(),
                value = performance.uploadConcurrency,
                step = 2,
                range = 1..PerformanceSettings.CUSTOM_MAX_UPLOAD_WORKERS,
                enabled = enabled,
                onChange = { onPerformanceChange(performance.copy(uploadConcurrency = it)) },
            )
            AdjustPerformanceValue(
                title = "Download workers",
                valueText = performance.downloadConcurrency.toString(),
                value = performance.downloadConcurrency,
                step = 2,
                range = 1..PerformanceSettings.CUSTOM_MAX_DOWNLOAD_WORKERS,
                enabled = enabled,
                onChange = { onPerformanceChange(performance.copy(downloadConcurrency = it)) },
            )
            SettingSwitch("Burst polling", "Check Drive faster just after traffic.", performance.burstPoll, enabled) {
                onPerformanceChange(performance.copy(burstPoll = it))
            }
        } else {
            SectionTitle("Current limits")
            SupportingText("Check interval: ${performance.pollMs} ms\n" +
                "Upload workers: ${performance.uploadConcurrency}\n" +
                "Download workers: ${performance.downloadConcurrency}\n" +
                "Burst polling: ${if (performance.burstPoll) "On" else "Off"}")
        }
        if (enabled) {
            HorizontalDivider(Modifier.padding(top = 16.dp, bottom = 4.dp))
            DriveActivity(metrics)
        }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun AdjustPerformanceValue(
    title: String,
    valueText: String,
    value: Int,
    step: Int,
    range: IntRange,
    enabled: Boolean,
    onChange: (Int) -> Unit,
) {
    FlowRow(
        Modifier.fillMaxWidth().padding(vertical = 4.dp),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        Text(title, modifier = Modifier.padding(top = 12.dp, end = 8.dp), style = MaterialTheme.typography.bodyLarge)
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            IconButton(onClick = { onChange((value - step).coerceIn(range)) }, enabled = enabled && value > range.first) {
                Icon(Icons.Rounded.Remove, "Decrease ${title.lowercase()}")
            }
            Text(valueText, Modifier.semantics { liveRegion = LiveRegionMode.Polite }, style = MaterialTheme.typography.bodyLarge)
            IconButton(onClick = { onChange((value + step).coerceIn(range)) }, enabled = enabled && value < range.last) {
                Icon(Icons.Rounded.Add, "Increase ${title.lowercase()}")
            }
        }
    }
}

@Composable
private fun DriveActivity(metrics: DriveMetrics?, waitingForSample: Boolean = false) {
    SectionTitle("Google Drive activity")
    if (metrics == null) {
        SupportingText(if (waitingForSample) "No measurements from this connection yet."
            else "Connect to measure this phone’s Drive activity.")
    } else {
        val pressure = drivePressureFraction(metrics.unitsPerMinute).takeIf { it.isFinite() } ?: 0f
        val units = NumberFormat.getIntegerInstance().format(metrics.unitsPerMinute)
        val label = if (metrics.unitsPerMinute == 0.0 && metrics.errorsPerMinute == 0.0) "Idle"
            else drivePressureLabel(metrics.unitsPerMinute, metrics.errorsPerMinute, metrics.lastErrorReason)
        val error = metrics.errorsPerMinute > 0.0 || metrics.backoffActive
        Text("$label · $units units/min", style = MaterialTheme.typography.bodyLarge,
            color = if (error) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface)
        LinearProgressIndicator(
            progress = { pressure },
            modifier = Modifier.fillMaxWidth().padding(vertical = 12.dp),
            color = if (error) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.primary,
        )
        if (metrics.ops.isNotBlank()) SupportingText(metrics.ops, Modifier.padding(bottom = 8.dp))
        if (metrics.backoffActive) {
            val cooldown = metrics.backoffReason.ifBlank { "rate limited" }
            Text("Drive cooldown: $cooldown · ${metrics.backoffWaitSeconds.toInt()} s remaining",
                modifier = Modifier.padding(bottom = 8.dp), style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.error)
        }
    }
    SupportingText("Measures requests from this phone. The exit and other clients use the same Google budget.",
        Modifier.padding(top = 4.dp))
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
internal fun LogsPage(
    logText: String,
    onRefresh: () -> Unit,
    onCopy: () -> Unit,
    modifier: Modifier = Modifier,
) {
    val logs = remember(logText) { if (logText.isBlank()) emptyList() else logText.lines() }
    LazyColumn(modifier.fillMaxSize(), contentPadding = PaddingValues(start = 24.dp, end = 24.dp, bottom = 24.dp)) {
        item {
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.padding(vertical = 8.dp)) {
                OutlinedButton(onClick = onRefresh, modifier = Modifier.heightIn(min = 48.dp)) {
                    Icon(Icons.Rounded.Refresh, contentDescription = null, modifier = Modifier.size(18.dp))
                    Text("Refresh", Modifier.padding(start = 8.dp))
                }
                OutlinedButton(onClick = onCopy, enabled = logs.isNotEmpty(), modifier = Modifier.heightIn(min = 48.dp)) {
                    Icon(Icons.Rounded.ContentCopy, contentDescription = null, modifier = Modifier.size(18.dp))
                    Text("Copy logs", Modifier.padding(start = 8.dp))
                }
            }
            HorizontalDivider(Modifier.padding(top = 4.dp, bottom = 12.dp))
            if (logs.isEmpty()) SupportingText("No logs yet. Try connecting, then refresh.",
                Modifier.semantics { liveRegion = LiveRegionMode.Polite })
        }
        items(logs) { line ->
            Text(line, Modifier.padding(vertical = 2.dp), style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant, fontFamily = FontFamily.Monospace)
        }
    }
}
