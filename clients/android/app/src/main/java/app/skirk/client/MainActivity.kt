package app.skirk.client

import android.Manifest
import android.app.Activity
import android.app.ActivityManager
import android.content.ClipboardManager
import android.content.Context
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.view.View
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Image
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Add
import androidx.compose.material.icons.rounded.AccountCircle
import androidx.compose.material.icons.rounded.Check
import androidx.compose.material.icons.rounded.CloudQueue
import androidx.compose.material.icons.rounded.ContentCopy
import androidx.compose.material.icons.rounded.ContentPaste
import androidx.compose.material.icons.rounded.DataUsage
import androidx.compose.material.icons.rounded.Delete
import androidx.compose.material.icons.rounded.PlayArrow
import androidx.compose.material.icons.rounded.PowerSettingsNew
import androidx.compose.material.icons.rounded.Refresh
import androidx.compose.material.icons.rounded.Settings
import androidx.compose.material.icons.rounded.Shield
import androidx.compose.material.icons.rounded.Speed
import androidx.compose.material.icons.rounded.Storage
import androidx.compose.material.icons.rounded.Tune
import androidx.compose.material.icons.rounded.VpnKey
import androidx.compose.material.icons.rounded.WifiTethering
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.SwitchDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.toArgb
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalView
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.delay
import java.io.File
import org.json.JSONObject

private val LightColors = lightColorScheme(
    primary = Color(0xFF111111),
    onPrimary = Color.White,
    surface = Color.White,
    background = Color(0xFFF6F6F6),
    onSurface = Color(0xFF111111),
    surfaceVariant = Color(0xFFF4F4F5),
    onSurfaceVariant = Color(0xFF71717A),
    outline = Color(0xFFE4E4E7),
)

private val DarkColors = darkColorScheme(
    primary = Color(0xFFF5F5F5),
    onPrimary = Color(0xFF111111),
    surface = Color(0xFF252526),
    background = Color(0xFF1E1E1E),
    onSurface = Color(0xFFF5F5F5),
    surfaceVariant = Color(0xFF2D2D30),
    onSurfaceVariant = Color(0xFFA7A7AD),
    outline = Color(0xFF3C3C3C),
)

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            SkirkTheme {
                ConfigScreen()
            }
        }
    }
}

@Composable
@Suppress("DEPRECATION")
private fun SkirkTheme(content: @Composable () -> Unit) {
    val dark = isSystemInDarkTheme()
    val colors = if (dark) DarkColors else LightColors
    val view = LocalView.current
    SideEffect {
        val window = (view.context as Activity).window
        window.statusBarColor = colors.background.toArgb()
        window.navigationBarColor = colors.background.toArgb()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            window.decorView.systemUiVisibility = if (dark) {
                0
            } else {
                View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR
            }
        }
    }
    MaterialTheme(colorScheme = colors, content = content)
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ConfigScreen() {
    val context = LocalContext.current
    val clipboard = LocalClipboardManager.current
    val store = remember(context) { ProfileStore(context.applicationContext) }
    val connectionStore = remember(context) { ConnectionStateStore(context.applicationContext) }
    var profiles by remember { mutableStateOf(store.listProfiles()) }
    var selectedId by remember { mutableStateOf(store.selectedProfileId()) }
    var connectionState by remember { mutableStateOf(connectionStore.read()) }
    var lastConnectionUpdateAt by remember { mutableStateOf(connectionState.updatedAtMillis) }
    var rawConfig by remember { mutableStateOf("") }
    var importError by remember { mutableStateOf("") }
    var profileName by remember { mutableStateOf("Skirk profile") }
    var socksPort by remember { mutableStateOf(ClientProfile.DEFAULT_SOCKS_PORT.toString()) }
    var httpPort by remember { mutableStateOf(ClientProfile.DEFAULT_HTTP_PORT.toString()) }
    var selectedMode by remember { mutableStateOf(ClientProfile.CONNECTION_MODE_VPN) }
    var proxyShareLan by remember { mutableStateOf(false) }
    val running = connectionState.running
    var message by remember { mutableStateOf(connectionState.message) }
    var logText by remember { mutableStateOf(readSkirkLogs(context, connectionState.mode)) }
    var driveMetrics by remember { mutableStateOf(readDriveMetrics(context, connectionState.mode)) }
    var diagnosticsExpanded by remember { mutableStateOf(false) }
    var pendingVpnProfile by remember { mutableStateOf<ClientProfile?>(null) }
    var importExpanded by remember { mutableStateOf(profiles.isEmpty()) }
    var advancedSettingsOpen by remember { mutableStateOf(false) }

    fun refreshConnectionState() {
        val raw = connectionStore.read()
        val oldEnoughToValidate = System.currentTimeMillis() - raw.updatedAtMillis > SERVICE_STATE_GRACE_MS
        if (raw.running && oldEnoughToValidate && !isSkirkServiceRunning(context, raw.mode)) {
            connectionStore.stopped("Disconnected")
        }
        val next = connectionStore.read()
        connectionState = next
        if (next.updatedAtMillis > lastConnectionUpdateAt) {
            lastConnectionUpdateAt = next.updatedAtMillis
            if (next.message.isNotBlank()) {
                message = next.message
            }
        }
    }

    fun refreshLogs(mode: String = connectionStore.read().mode) {
        logText = readSkirkLogs(context, mode)
        driveMetrics = readDriveMetrics(context, mode)
    }

    val notificationPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) {}
    val vpnPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { result ->
        val profile = pendingVpnProfile
        pendingVpnProfile = null
        if (result.resultCode == Activity.RESULT_OK && profile != null) {
            if (connectionStore.read().mode == ClientProfile.CONNECTION_MODE_PROXY) {
                SkirkProxyService.stop(context)
            }
            connectionStore.connecting(profile, "VPN connecting")
            refreshConnectionState()
            SkirkVpnService.start(context, profile)
            message = "VPN connecting"
        } else {
            connectionStore.stopped("VPN permission was not granted")
            refreshConnectionState()
            message = "VPN permission was not granted"
        }
    }

    fun refresh() {
        profiles = store.listProfiles()
        selectedId = store.selectedProfileId()
    }

    fun startProfile(profile: ClientProfile, mode: String, shareLan: Boolean) {
        val normalizedMode = ClientProfile.normalizeConnectionMode(mode)
        val runtimeProfile = profile.copy(
            connectionMode = normalizedMode,
            shareLan = normalizedMode == ClientProfile.CONNECTION_MODE_PROXY && shareLan,
        )
        store.saveProfile(runtimeProfile)
        refresh()
        if (runtimeProfile.connectionMode == ClientProfile.CONNECTION_MODE_VPN) {
            if (connectionStore.read().mode == ClientProfile.CONNECTION_MODE_PROXY) {
                SkirkProxyService.stop(context)
            }
            val intent = VpnService.prepare(context)
            if (intent != null) {
                pendingVpnProfile = runtimeProfile
                vpnPermission.launch(intent)
            } else {
                connectionStore.connecting(runtimeProfile, "VPN connecting")
                refreshConnectionState()
                SkirkVpnService.start(context, runtimeProfile)
                message = "VPN connecting"
            }
        } else {
            if (connectionStore.read().mode == ClientProfile.CONNECTION_MODE_VPN) {
                SkirkVpnService.stop(context)
            }
            connectionStore.connecting(runtimeProfile, "Proxy connecting on ${proxyAddress(runtimeProfile, runtimeProfile.shareLan)}")
            refreshConnectionState()
            SkirkProxyService.start(context, runtimeProfile)
            message = "Proxy connecting on ${proxyAddress(runtimeProfile, runtimeProfile.shareLan)}"
        }
    }

    fun disconnectActive(reason: String = "Disconnected") {
        when (connectionStore.read().mode) {
            ClientProfile.CONNECTION_MODE_VPN -> SkirkVpnService.stop(context)
            ClientProfile.CONNECTION_MODE_PROXY -> SkirkProxyService.stop(context)
            else -> {
                SkirkVpnService.stop(context)
                SkirkProxyService.stop(context)
            }
        }
        connectionStore.stopped(reason)
        refreshConnectionState()
        message = reason
    }

    LaunchedEffect(Unit) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    LaunchedEffect(Unit) {
        while (true) {
            refreshConnectionState()
            val next = connectionStore.read()
            if (next.running) {
                refreshLogs(next.mode)
            }
            delay(2_000L)
        }
    }

    val selected = profiles.firstOrNull { it.id == selectedId } ?: profiles.firstOrNull()
    LaunchedEffect(selected?.id) {
        selectedMode = selected?.connectionMode ?: ClientProfile.CONNECTION_MODE_VPN
        proxyShareLan = selected?.shareLan ?: false
    }
    LaunchedEffect(profiles.isEmpty()) {
        if (profiles.isEmpty()) {
            importExpanded = true
        }
    }

    fun pasteProfileFromClipboard() {
        val clipboard = context.getSystemService(ClipboardManager::class.java)
        rawConfig = clipboard.primaryClip?.getItemAt(0)?.coerceToText(context)?.toString().orEmpty()
        importError = ""
    }

    fun importProfile() {
        try {
            val port = socksPort.toIntOrNull()
                ?.let { ClientProfile.validateSocksPort(it) }
                ?: error("Local SOCKS port is required")
            val httpProxyPort = httpPort.toIntOrNull()
                ?.let { ClientProfile.validateHttpPort(it, port) }
                ?: error("Local HTTP proxy port is required")
            val profile = ClientProfile.fromRawConfig(
                name = profileName,
                rawConfig = rawConfig,
                socksPort = port,
                httpPort = httpProxyPort,
                shareLan = false,
                connectionMode = ClientProfile.CONNECTION_MODE_VPN,
            )
            store.saveProfile(profile)
            rawConfig = ""
            importError = ""
            selectedMode = profile.connectionMode
            proxyShareLan = false
            importExpanded = false
            message = "Imported ${profile.name}"
            refresh()
        } catch (error: Exception) {
            val nextError = error.message ?: "Import failed"
            importError = nextError
            message = nextError
            Toast.makeText(context, nextError, Toast.LENGTH_LONG).show()
        }
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = {
            TopAppBar(
                title = {
                    Row(
                        horizontalArrangement = Arrangement.spacedBy(10.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Surface(
                            modifier = Modifier.size(34.dp),
                            shape = RoundedCornerShape(8.dp),
                            border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
                            color = Color.White,
                        ) {
                            Image(
                                painter = painterResource(R.drawable.logo_mark),
                                contentDescription = "Skirk",
                                contentScale = ContentScale.Fit,
                                modifier = Modifier.padding(5.dp),
                            )
                        }
                        Column {
                            Text("Skirk", fontWeight = FontWeight.SemiBold)
                            Text(
                                "v${BuildConfig.VERSION_NAME}",
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                style = MaterialTheme.typography.labelMedium,
                            )
                        }
                    }
                },
                colors = TopAppBarDefaults.topAppBarColors(
                    containerColor = MaterialTheme.colorScheme.background,
                    titleContentColor = MaterialTheme.colorScheme.onSurface,
                    actionIconContentColor = MaterialTheme.colorScheme.onSurface,
                ),
            )
        },
    ) { innerPadding ->
        LazyColumn(
            modifier = Modifier
                .fillMaxSize()
                .padding(innerPadding)
                .padding(horizontal = 16.dp, vertical = 12.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            if (profiles.isEmpty()) {
                item {
                    ImportPanel(
                        profileName = profileName,
                        socksPort = socksPort,
                        httpPort = httpPort,
                        rawConfig = rawConfig,
                        importError = importError,
                        onProfileNameChange = { profileName = it },
                        onSocksPortChange = { socksPort = it.filter(Char::isDigit).take(5) },
                        onHttpPortChange = { httpPort = it.filter(Char::isDigit).take(5) },
                        onRawConfigChange = {
                            rawConfig = it
                            importError = ""
                        },
                        onPaste = ::pasteProfileFromClipboard,
                        onImport = ::importProfile,
                    )
                }
            } else {
                item {
                    ProfilesPanel(
                        profiles = profiles,
                        selectedId = selected?.id,
                        running = running,
                        onSelect = { profile ->
                            store.selectProfile(profile.id)
                            selectedMode = profile.connectionMode
                            proxyShareLan = profile.shareLan
                            refresh()
                        },
                        onDelete = { profile ->
                            if (running && selected?.id == profile.id) {
                                disconnectActive()
                            }
                            store.deleteProfile(profile.id)
                            refresh()
                        },
                    )
                }
                item {
                    if (importExpanded) {
                        ImportPanel(
                            profileName = profileName,
                            socksPort = socksPort,
                            httpPort = httpPort,
                            rawConfig = rawConfig,
                            importError = importError,
                            onProfileNameChange = { profileName = it },
                            onSocksPortChange = { socksPort = it.filter(Char::isDigit).take(5) },
                            onHttpPortChange = { httpPort = it.filter(Char::isDigit).take(5) },
                            onRawConfigChange = {
                                rawConfig = it
                                importError = ""
                            },
                            onPaste = ::pasteProfileFromClipboard,
                            onImport = ::importProfile,
                        )
                    } else {
                        AddProfilePanel(onExpand = { importExpanded = true })
                    }
                }
            }

            item {
                ConnectionPanel(
                    selected = selected,
                    selectedMode = selectedMode,
                    proxyShareLan = proxyShareLan,
                    running = running,
                    message = message,
                    metrics = driveMetrics,
                    onModeChange = { selectedMode = it },
                    onProxyShareLanChange = { proxyShareLan = it },
                    onOpenAdvanced = { advancedSettingsOpen = true },
                    onConnect = { selected?.let { startProfile(it, selectedMode, proxyShareLan) } },
                    onDisconnect = { disconnectActive() },
                )
            }

            item {
                DiagnosticsPanel(
                    logText = logText,
                    expanded = diagnosticsExpanded,
                    onToggleExpanded = { diagnosticsExpanded = !diagnosticsExpanded },
                    onRefresh = { refreshLogs() },
                    onCopy = {
                        clipboard.setText(AnnotatedString(logText))
                        Toast.makeText(context, "Logs copied", Toast.LENGTH_SHORT).show()
                    },
                )
            }
        }
        if (advancedSettingsOpen) {
            AdvancedSettingsDialog(
                performance = selected?.performance ?: PerformanceSettings.recommended(),
                metrics = driveMetrics,
                enabled = selected != null && !running,
                onDismiss = { advancedSettingsOpen = false },
                onPerformanceChange = { performance ->
                    selected?.let { profile ->
                        store.saveProfile(profile.copy(performance = performance))
                        refresh()
                    }
                },
            )
        }
    }
}

@Composable
private fun ConnectionPanel(
    selected: ClientProfile?,
    selectedMode: String,
    proxyShareLan: Boolean,
    running: Boolean,
    message: String,
    metrics: DriveMetrics?,
    onModeChange: (String) -> Unit,
    onProxyShareLanChange: (Boolean) -> Unit,
    onOpenAdvanced: () -> Unit,
    onConnect: () -> Unit,
    onDisconnect: () -> Unit,
) {
    val uiState = connectionUiState(running, selected, message)
    Panel {
        Row(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.spacedBy(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                Text(
                    text = uiState.title,
                    style = MaterialTheme.typography.headlineSmall,
                    fontWeight = FontWeight.SemiBold,
                )
                Text(
                    text = connectionDetail(
                        state = uiState,
                        selected = selected,
                        selectedMode = selectedMode,
                        proxyShareLan = proxyShareLan,
                        message = message,
                    ),
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    style = MaterialTheme.typography.bodyMedium,
                )
            }
        }
        Button(
            onClick = if (running) onDisconnect else onConnect,
            enabled = running || selected != null,
            modifier = Modifier.fillMaxWidth(),
            colors = ButtonDefaults.buttonColors(
                containerColor = MaterialTheme.colorScheme.primary,
                contentColor = MaterialTheme.colorScheme.onPrimary,
            ),
        ) {
            Icon(
                if (running) Icons.Rounded.PowerSettingsNew else Icons.Rounded.PlayArrow,
                contentDescription = null,
            )
            Text(if (running) "Disconnect" else "Connect")
        }
        DriveUsageSummary(
            metrics = metrics,
            performance = selected?.performance ?: PerformanceSettings.recommended(),
            onOpenAdvanced = onOpenAdvanced,
        )
        HorizontalDivider(color = MaterialTheme.colorScheme.outline)
        SectionHeader(Icons.Rounded.PowerSettingsNew, "Connection mode", modeLabel(selectedMode))
        ModeSelector(
            selectedMode = selectedMode,
            enabled = selected != null && !running,
            onModeChange = onModeChange,
        )
        if (selectedMode == ClientProfile.CONNECTION_MODE_PROXY) {
            SwitchRow(
                title = "Share on LAN",
                detail = proxyAddress(selected, proxyShareLan),
                checked = proxyShareLan,
                enabled = !running,
                onCheckedChange = onProxyShareLanChange,
            )
        } else {
            InfoRow(Icons.Rounded.VpnKey, "VPN mode", "Routes Android app traffic through Skirk.")
        }
    }
}

@Composable
private fun AdvancedSettingsDialog(
    performance: PerformanceSettings,
    metrics: DriveMetrics?,
    enabled: Boolean,
    onDismiss: () -> Unit,
    onPerformanceChange: (PerformanceSettings) -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        confirmButton = {
            TextButton(onClick = onDismiss) {
                Text("Done")
            }
        },
        icon = { Icon(Icons.Rounded.Tune, contentDescription = null) },
        title = { Text("Advanced settings") },
        text = {
            Column(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(max = 520.dp)
                    .verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                DriveUsageDetails(metrics = metrics, performance = performance)
                HorizontalDivider(color = MaterialTheme.colorScheme.outline)
                PerformanceSettingsPanel(
                    performance = performance,
                    enabled = enabled,
                    onPerformanceChange = onPerformanceChange,
                )
            }
        },
    )
}

@Composable
private fun PerformanceSettingsPanel(
    performance: PerformanceSettings,
    enabled: Boolean,
    onPerformanceChange: (PerformanceSettings) -> Unit,
) {
    SectionHeader(Icons.Rounded.Speed, "Performance", performancePresetLabel(performance.preset))
    listOf(
        listOf(PerformanceSettings.PRESET_LOWER_USAGE, PerformanceSettings.PRESET_RECOMMENDED),
        listOf(PerformanceSettings.PRESET_RESPONSIVE, PerformanceSettings.PRESET_BULK_TRANSFER),
    ).forEach { row ->
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.fillMaxWidth()) {
            row.forEach { preset ->
                ModeCard(
                    icon = Icons.Rounded.Speed,
                    title = performancePresetLabel(preset),
                    subtitle = performancePresetDetail(PerformanceSettings.forPreset(preset)),
                    selected = performance.preset == preset,
                    enabled = enabled,
                    modifier = Modifier.weight(1f),
                    onClick = { onPerformanceChange(PerformanceSettings.forPreset(preset)) },
                )
            }
        }
    }
    OutlinedButton(
        onClick = { onPerformanceChange(performance.copy(preset = PerformanceSettings.PRESET_CUSTOM)) },
        enabled = enabled,
        modifier = Modifier.fillMaxWidth(),
    ) {
        Icon(Icons.Rounded.Settings, contentDescription = null)
        Text("Custom keeps current values")
    }
    if (performance.preset == PerformanceSettings.PRESET_CUSTOM) {
        AdjustValueRow(
            title = "Check interval",
            detail = "Higher values use fewer Drive list calls.",
            valueText = "${performance.pollMs} ms",
            enabled = enabled,
            onDecrease = {
                onPerformanceChange(performance.copy(pollMs = (performance.pollMs - 250).coerceAtLeast(PerformanceSettings.CUSTOM_MIN_POLL_MS)))
            },
            onIncrease = {
                onPerformanceChange(performance.copy(pollMs = (performance.pollMs + 250).coerceAtMost(60000)))
            },
        )
        AdjustValueRow(
            title = "Upload workers",
            detail = "More workers can open streams faster but spend quota sooner.",
            valueText = performance.uploadConcurrency.toString(),
            enabled = enabled,
            onDecrease = {
                onPerformanceChange(performance.copy(uploadConcurrency = (performance.uploadConcurrency - 2).coerceAtLeast(1)))
            },
            onIncrease = {
                onPerformanceChange(performance.copy(uploadConcurrency = (performance.uploadConcurrency + 2).coerceAtMost(PerformanceSettings.CUSTOM_MAX_UPLOAD_WORKERS)))
            },
        )
        AdjustValueRow(
            title = "Download workers",
            detail = "More workers help bulk downloads and can crowd interactive traffic.",
            valueText = performance.downloadConcurrency.toString(),
            enabled = enabled,
            onDecrease = {
                onPerformanceChange(performance.copy(downloadConcurrency = (performance.downloadConcurrency - 2).coerceAtLeast(1)))
            },
            onIncrease = {
                onPerformanceChange(performance.copy(downloadConcurrency = (performance.downloadConcurrency + 2).coerceAtMost(PerformanceSettings.CUSTOM_MAX_DOWNLOAD_WORKERS)))
            },
        )
        SwitchRow(
            title = "Burst polling",
            detail = "Faster short window after traffic; useful but costly.",
            checked = performance.burstPoll,
            enabled = enabled,
            onCheckedChange = { onPerformanceChange(performance.copy(burstPoll = it)) },
        )
    }
    InfoRow(
        Icons.Rounded.Tune,
        "Current limits",
        "${performance.pollMs}ms check · ${performance.uploadConcurrency} up · ${performance.downloadConcurrency} down · ${if (performance.burstPoll) "burst on" else "burst off"}",
    )
    if (performance.burstPoll) {
        InfoRow(
            Icons.Rounded.CloudQueue,
            "Drive API warning",
            "Burst polling checks faster after traffic and can burn list quota quickly.",
        )
    }
    if (performance.preset == PerformanceSettings.PRESET_CUSTOM) {
        InfoRow(
            Icons.Rounded.Tune,
            "Custom warning",
            "Low check intervals and high workers can burn Drive quota quickly.",
        )
    }
}

@Composable
private fun AdjustValueRow(
    title: String,
    detail: String,
    valueText: String,
    enabled: Boolean,
    onDecrease: () -> Unit,
    onIncrease: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(8.dp))
            .padding(12.dp),
        horizontalArrangement = Arrangement.spacedBy(10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(title, fontWeight = FontWeight.Medium)
            Text(detail, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        OutlinedButton(onClick = onDecrease, enabled = enabled) {
            Text("-")
        }
        Text(valueText, fontWeight = FontWeight.SemiBold)
        OutlinedButton(onClick = onIncrease, enabled = enabled) {
            Text("+")
        }
    }
}

@Composable
private fun DriveUsageSummary(
    metrics: DriveMetrics?,
    performance: PerformanceSettings,
    onOpenAdvanced: () -> Unit,
) {
    val displayUnits = metrics?.unitsPerMinute ?: estimatedIdleUnits(performance)
    val errors = metrics?.errorsPerMinute ?: 0.0
    val label = drivePressureLabel(displayUnits, errors, metrics?.lastErrorReason.orEmpty())
    val percent = drivePressurePercent(displayUnits)
    Surface(
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
        color = MaterialTheme.colorScheme.surfaceVariant,
    ) {
        Column(
            modifier = Modifier.padding(12.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween,
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Rounded.DataUsage, contentDescription = null, modifier = Modifier.size(18.dp))
                    Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
                        Text("Drive API pressure", fontWeight = FontWeight.Medium)
                        Text(
                            if (metrics == null) "Idle estimate from this phone." else "Measured from this phone.",
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            style = MaterialTheme.typography.bodySmall,
                        )
                    }
                }
                TextButton(onClick = onOpenAdvanced) {
                    Icon(Icons.Rounded.Settings, contentDescription = null)
                    Text("Advanced")
                }
            }
            QuotaBar(unitsPerMinute = displayUnits, errorsPerMinute = errors)
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween,
            ) {
                Text("$label · $percent%", fontWeight = FontWeight.SemiBold)
                Text(
                    "~${displayUnits.toInt()} units/min",
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    style = MaterialTheme.typography.bodyMedium,
                )
            }
            Text(
                "Performance: ${performancePresetLabel(performance.preset)} · ${performancePresetDetail(performance)}",
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                style = MaterialTheme.typography.bodySmall,
            )
            if (metrics?.backoffActive == true) {
                Text(
                    "Drive cooldown: ${metrics.backoffReason.ifBlank { "rate limited" }} · ${metrics.backoffWaitSeconds.toInt()}s",
                    color = MaterialTheme.colorScheme.error,
                    style = MaterialTheme.typography.bodySmall,
                )
            }
        }
    }
}

@Composable
private fun DriveUsageDetails(metrics: DriveMetrics?, performance: PerformanceSettings) {
    val idleEstimate = estimatedIdleUnits(performance)
    val measuredUnits = metrics?.unitsPerMinute
    Column(verticalArrangement = Arrangement.spacedBy(10.dp)) {
        SectionHeader(
            Icons.Rounded.CloudQueue,
            "Drive API usage",
            measuredUnits?.let { drivePressureLabel(it, metrics.errorsPerMinute, metrics.lastErrorReason) } ?: "Estimate",
        )
        QuotaBar(unitsPerMinute = measuredUnits ?: idleEstimate, errorsPerMinute = metrics?.errorsPerMinute ?: 0.0)
        InfoRow(
            Icons.Rounded.DataUsage,
            "Current phone",
            measuredUnits?.let { "${it.toInt()} units/min · ${metrics.ops.ifBlank { "no ops yet" }}" }
                ?: "Connect to measure this phone's local Drive API usage.",
        )
        InfoRow(
            Icons.Rounded.Speed,
            "Idle estimate",
            "~${idleEstimate.toInt()} units/min from the selected check interval.",
        )
        InfoRow(
            Icons.Rounded.CloudQueue,
            "What this means",
            "Skirk uses Google Drive requests, not Drive storage. The exit and other clients share the same Google budget.",
        )
        if (metrics?.backoffActive == true) {
            InfoRow(
                Icons.Rounded.CloudQueue,
                "Drive cooldown",
                "${metrics.backoffReason.ifBlank { "rate limited" }} · ${metrics.backoffWaitSeconds.toInt()}s remaining",
            )
        }
    }
}

@Composable
private fun QuotaBar(unitsPerMinute: Double, errorsPerMinute: Double) {
    val pressure = drivePressureFraction(unitsPerMinute)
    val color = when {
        errorsPerMinute > 0.0 -> Color(0xFFDC2626)
        pressure >= 0.7f -> Color(0xFFEA580C)
        pressure >= 0.3f -> Color(0xFFEAB308)
        else -> Color(0xFF22C55E)
    }
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(9.dp)
            .background(MaterialTheme.colorScheme.outline, RoundedCornerShape(999.dp)),
    ) {
        Box(
            modifier = Modifier
                .fillMaxWidth(pressure.coerceIn(0.03f, 1f))
                .height(9.dp)
                .background(color, RoundedCornerShape(999.dp)),
        )
    }
}

@Composable
private fun DiagnosticsPanel(
    logText: String,
    expanded: Boolean,
    onToggleExpanded: () -> Unit,
    onRefresh: () -> Unit,
    onCopy: () -> Unit,
) {
    Panel {
        Row(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Rounded.Storage, contentDescription = null, modifier = Modifier.size(18.dp))
                    Text("Diagnostics", fontWeight = FontWeight.SemiBold)
                }
                Text(
                    if (expanded) "Sidecar logs and support capture" else "Logs are collapsed until you need them.",
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    style = MaterialTheme.typography.bodyMedium,
                )
            }
            TextButton(onClick = onToggleExpanded) {
                Text(if (expanded) "Hide" else "Show")
            }
        }
        if (expanded) {
            Surface(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 120.dp, max = 260.dp),
                shape = RoundedCornerShape(8.dp),
                color = MaterialTheme.colorScheme.surfaceVariant,
                border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
            ) {
                Text(
                    text = logText.ifBlank { "No logs yet." },
                    modifier = Modifier
                        .verticalScroll(rememberScrollState())
                        .padding(12.dp),
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    style = MaterialTheme.typography.bodySmall,
                )
            }
            Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                OutlinedButton(onClick = onRefresh) {
                    Icon(Icons.Rounded.Refresh, contentDescription = null)
                    Text("Refresh")
                }
                OutlinedButton(onClick = onCopy, enabled = logText.isNotBlank()) {
                    Icon(Icons.Rounded.ContentCopy, contentDescription = null)
                    Text("Copy")
                }
            }
        } else {
            InfoRow(
                Icons.Rounded.Storage,
                "Diagnostics are secondary",
                "Open this section for startup failures, tunnel output, or support capture.",
            )
        }
    }
}

@Composable
private fun AddProfilePanel(onExpand: () -> Unit) {
    Panel {
        Row(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Row(
                modifier = Modifier.weight(1f),
                horizontalArrangement = Arrangement.spacedBy(8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Icon(Icons.Rounded.Add, contentDescription = null, modifier = Modifier.size(18.dp))
                Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
                    Text("Add profile", fontWeight = FontWeight.SemiBold)
                    Text(
                        "Import another config",
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        style = MaterialTheme.typography.bodyMedium,
                    )
                }
            }
            OutlinedButton(onClick = onExpand) {
                Icon(Icons.Rounded.Add, contentDescription = null)
                Text("Add")
            }
        }
    }
}

@Composable
private fun ImportPanel(
    profileName: String,
    socksPort: String,
    httpPort: String,
    rawConfig: String,
    importError: String,
    onProfileNameChange: (String) -> Unit,
    onSocksPortChange: (String) -> Unit,
    onHttpPortChange: (String) -> Unit,
    onRawConfigChange: (String) -> Unit,
    onPaste: () -> Unit,
    onImport: () -> Unit,
) {
    Panel {
        SectionHeader(Icons.Rounded.Add, "Import profile", "One-line config")
        OutlinedTextField(
            value = rawConfig,
            onValueChange = onRawConfigChange,
            modifier = Modifier.fillMaxWidth(),
            minLines = 5,
            label = { Text("Skirk profile or client.json") },
            isError = importError.isNotBlank(),
            supportingText = {
                if (importError.isNotBlank()) {
                    Text(importError)
                } else {
                    Text("Paste the one-line skirk: profile or generated client.json.")
                }
            },
        )
        Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
            Button(
                onClick = onImport,
                enabled = rawConfig.isNotBlank(),
                colors = ButtonDefaults.buttonColors(
                    containerColor = MaterialTheme.colorScheme.primary,
                    contentColor = MaterialTheme.colorScheme.onPrimary,
                ),
            ) {
                Icon(Icons.Rounded.Add, contentDescription = null)
                Text("Import")
            }
            OutlinedButton(onClick = onPaste) {
                Icon(Icons.Rounded.ContentPaste, contentDescription = null)
                Text("Paste")
            }
        }
        OutlinedTextField(
            value = profileName,
            onValueChange = onProfileNameChange,
            modifier = Modifier.fillMaxWidth(),
            label = { Text("Profile name") },
            singleLine = true,
        )
        OutlinedTextField(
            value = socksPort,
            onValueChange = onSocksPortChange,
            modifier = Modifier.fillMaxWidth(),
            label = { Text("Local SOCKS port") },
            singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
        )
        OutlinedTextField(
            value = httpPort,
            onValueChange = onHttpPortChange,
            modifier = Modifier.fillMaxWidth(),
            label = { Text("Local HTTP proxy port") },
            singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
        )
    }
}

@Composable
private fun ProfilesPanel(
    profiles: List<ClientProfile>,
    selectedId: String?,
    running: Boolean,
    onSelect: (ClientProfile) -> Unit,
    onDelete: (ClientProfile) -> Unit,
) {
    Panel {
        SectionHeader(Icons.Rounded.AccountCircle, "Profiles", "${profiles.size} saved")
        if (profiles.isEmpty()) {
            EmptyState()
        } else {
            profiles.forEach { profile ->
                ProfileRow(
                    profile = profile,
                    selected = profile.id == selectedId,
                    enabled = !running,
                    onSelect = { onSelect(profile) },
                    onDelete = { onDelete(profile) },
                )
            }
        }
    }
}

@Composable
private fun Panel(content: @Composable ColumnScope.() -> Unit) {
    Surface(
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
        color = MaterialTheme.colorScheme.surface,
        tonalElevation = 0.dp,
    ) {
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
            content = content,
        )
    }
}

@Composable
private fun SectionHeader(icon: ImageVector, title: String, detail: String, modifier: Modifier = Modifier) {
    Row(
        modifier = modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
            Icon(icon, contentDescription = null, modifier = Modifier.size(18.dp))
            Text(title, fontWeight = FontWeight.SemiBold)
        }
        Text(detail, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun ModeSelector(
    selectedMode: String,
    enabled: Boolean,
    onModeChange: (String) -> Unit,
) {
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        ModeCard(
            icon = Icons.Rounded.VpnKey,
            title = "VPN",
            subtitle = "All apps",
            selected = selectedMode == ClientProfile.CONNECTION_MODE_VPN,
            enabled = enabled,
            modifier = Modifier.weight(1f),
            onClick = { onModeChange(ClientProfile.CONNECTION_MODE_VPN) },
        )
        ModeCard(
            icon = Icons.Rounded.WifiTethering,
            title = "Proxy",
            subtitle = "SOCKS5 + HTTP",
            selected = selectedMode == ClientProfile.CONNECTION_MODE_PROXY,
            enabled = enabled,
            modifier = Modifier.weight(1f),
            onClick = { onModeChange(ClientProfile.CONNECTION_MODE_PROXY) },
        )
    }
}

@Composable
private fun ModeCard(
    icon: ImageVector,
    title: String,
    subtitle: String,
    selected: Boolean,
    enabled: Boolean,
    modifier: Modifier = Modifier,
    onClick: () -> Unit,
) {
    Surface(
        modifier = modifier.selectable(
            selected = selected,
            enabled = enabled,
            role = Role.RadioButton,
            onClick = onClick,
        ),
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(
            1.dp,
            if (selected) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.outline,
        ),
        color = if (selected) MaterialTheme.colorScheme.surfaceVariant else MaterialTheme.colorScheme.surface,
    ) {
        Column(
            modifier = Modifier.padding(14.dp),
            verticalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            Icon(icon, contentDescription = null, modifier = Modifier.size(18.dp))
            Text(title, fontWeight = FontWeight.SemiBold)
            Text(subtitle, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
private fun SwitchRow(
    title: String,
    detail: String,
    checked: Boolean,
    enabled: Boolean,
    onCheckedChange: (Boolean) -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(8.dp))
            .padding(12.dp),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(title, fontWeight = FontWeight.Medium)
            Text(detail, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Switch(
            checked = checked,
            enabled = enabled,
            onCheckedChange = onCheckedChange,
            colors = SwitchDefaults.colors(
                checkedTrackColor = MaterialTheme.colorScheme.primary,
                checkedThumbColor = MaterialTheme.colorScheme.onPrimary,
            ),
        )
    }
}

@Composable
private fun InfoRow(icon: ImageVector, title: String, detail: String) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(8.dp))
            .padding(12.dp),
        horizontalArrangement = Arrangement.spacedBy(10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(icon, contentDescription = null, modifier = Modifier.size(18.dp))
        Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(title, fontWeight = FontWeight.Medium)
            Text(detail, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
private fun ProfileRow(
    profile: ClientProfile,
    selected: Boolean,
    enabled: Boolean,
    onSelect: () -> Unit,
    onDelete: () -> Unit,
) {
    Surface(
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(
            1.dp,
            if (selected) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.outline,
        ),
        color = if (selected) MaterialTheme.colorScheme.surfaceVariant else MaterialTheme.colorScheme.surface,
    ) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .padding(12.dp),
            horizontalArrangement = Arrangement.spacedBy(10.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                    if (selected) {
                        Icon(Icons.Rounded.Check, contentDescription = null, modifier = Modifier.size(16.dp))
                    } else {
                        Icon(Icons.Rounded.AccountCircle, contentDescription = null, modifier = Modifier.size(16.dp))
                    }
                    Text(profile.name, fontWeight = FontWeight.SemiBold)
                }
                Text(
                    "${profile.routeMode} / SOCKS ${profile.socksAddress} / HTTP ${profile.httpAddress}",
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            if (!selected) {
                OutlinedButton(onClick = onSelect, enabled = enabled) {
                    Text("Select")
                }
            }
            IconButton(onClick = onDelete, enabled = enabled) {
                Icon(Icons.Rounded.Delete, contentDescription = "Delete ${profile.name}")
            }
        }
    }
}

@Composable
private fun EmptyState() {
    Surface(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
        color = MaterialTheme.colorScheme.surfaceVariant,
    ) {
        Column(
            modifier = Modifier.padding(18.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Icon(Icons.Rounded.Storage, contentDescription = null)
            Text("No profiles yet", color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

private enum class ConnectionUiState(val title: String) {
    Setup("Setup"),
    Ready("Ready"),
    Connecting("Connecting"),
    Connected("Connected"),
    Error("Needs attention"),
}

private fun connectionUiState(running: Boolean, selected: ClientProfile?, message: String): ConnectionUiState = when {
    selected == null -> ConnectionUiState.Setup
    running && message.contains("connecting", ignoreCase = true) -> ConnectionUiState.Connecting
    running -> ConnectionUiState.Connected
    isConnectionErrorMessage(message) -> ConnectionUiState.Error
    else -> ConnectionUiState.Ready
}

private fun connectionDetail(
    state: ConnectionUiState,
    selected: ClientProfile?,
    selectedMode: String,
    proxyShareLan: Boolean,
    message: String,
): String {
    if (selected == null) {
        return "Import a profile below to enable connection."
    }
    val actionableMessage = connectionActionableMessage(message)
    if (state == ConnectionUiState.Error && actionableMessage != null) {
        return actionableMessage
    }
    return "${selected.name} · ${connectionRouteDetail(selected, selectedMode, proxyShareLan)}"
}

private fun connectionActionableMessage(message: String): String? {
    val normalized = message.trim()
    if (normalized.isBlank()) {
        return null
    }
    val lower = normalized.lowercase()
    if (
        lower == "disconnected" ||
        lower == "vpn connected" ||
        lower.startsWith("socks connected") ||
        lower.startsWith("proxy connected") ||
        lower.endsWith("stopped") ||
        lower.startsWith("imported ")
    ) {
        return null
    }
    return normalized
}

private fun isConnectionErrorMessage(message: String): Boolean {
    val lower = message.trim().lowercase()
    return lower.contains("failed") ||
        lower.contains("error") ||
        lower.contains("not granted") ||
        lower.contains("revoked")
}

private fun modeLabel(mode: String): String =
    if (mode == ClientProfile.CONNECTION_MODE_PROXY) "Proxy" else "VPN"

private fun routeSummary(
    selected: ClientProfile?,
    selectedMode: String,
    proxyShareLan: Boolean,
): String = when {
    selected == null -> "Not configured"
    selectedMode == ClientProfile.CONNECTION_MODE_PROXY && proxyShareLan -> "LAN enabled"
    selectedMode == ClientProfile.CONNECTION_MODE_PROXY -> "Local proxy"
    else -> "Device VPN"
}

private fun connectionRouteDetail(
    selected: ClientProfile,
    selectedMode: String,
    proxyShareLan: Boolean,
): String = when {
    selectedMode == ClientProfile.CONNECTION_MODE_PROXY && proxyShareLan ->
        "LAN proxy · ${proxyEndpoints(selected, true)}"
    selectedMode == ClientProfile.CONNECTION_MODE_PROXY ->
        "Local proxy · ${proxyEndpoints(selected, false)}"
    else -> "Device VPN · no manual proxy setup"
}

private fun proxyAddress(profile: ClientProfile?, shareLan: Boolean): String {
    if (profile == null) {
        return "Import or select a profile first."
    }
    if (!shareLan) {
        return "This phone only: ${proxyEndpoints(profile, false)}"
    }
    return "Use only on trusted networks: ${proxyEndpoints(profile, true)}"
}

private fun proxyEndpoints(profile: ClientProfile, shareLan: Boolean): String {
    if (!shareLan) {
        return "SOCKS5 ${profile.socksAddress} · HTTP ${profile.httpAddress}"
    }
    val socksAddress = AndroidSkirkEngine.lanAddresses(profile.socksPort)
        .firstOrNull()
        ?: "0.0.0.0:${profile.socksPort}"
    val httpAddress = AndroidSkirkEngine.lanAddresses(profile.httpPort)
        .firstOrNull()
        ?: "0.0.0.0:${profile.httpPort}"
    return "SOCKS5 $socksAddress · HTTP $httpAddress"
}

@Suppress("DEPRECATION")
private fun isSkirkServiceRunning(context: Context, mode: String): Boolean {
    val manager = context.getSystemService(ActivityManager::class.java) ?: return true
    val expectedClass = when (mode) {
        ClientProfile.CONNECTION_MODE_PROXY -> SkirkProxyService::class.java.name
        ClientProfile.CONNECTION_MODE_VPN -> SkirkVpnService::class.java.name
        else -> return false
    }
    return manager.getRunningServices(Int.MAX_VALUE)
        .any { service -> service.service.className == expectedClass }
}

private fun readSkirkLogs(context: Context, activeMode: String): String {
    val logsDir = File(context.filesDir, "logs")
    if (!logsDir.exists()) {
        return ""
    }
    val files = logsDir.listFiles()
        ?.filter { it.isFile && it.name.endsWith(".log") }
        .orEmpty()
    val ordered = when (activeMode) {
        ClientProfile.CONNECTION_MODE_PROXY -> files.filter { it.name == "skirk-client.log" }
        ClientProfile.CONNECTION_MODE_VPN -> files.filter { it.name == "skirk-vpn-client.log" }
        else -> files.sortedBy { it.name }
    }
    return ordered
        .joinToString("\n\n") { file ->
            val text = file.readTail(maxBytes = 64 * 1024, maxLines = 240)
            "== ${file.name} ==\n$text"
        }
        .takeLast(96 * 1024)
}

private data class DriveMetrics(
    val unitsPerMinute: Double,
    val errorsPerMinute: Double,
    val lastErrorReason: String,
    val ops: String,
    val backoffActive: Boolean,
    val backoffReason: String,
    val backoffWaitSeconds: Double,
)

private fun readDriveMetrics(context: Context, activeMode: String): DriveMetrics? {
    val logsDir = File(context.filesDir, "logs")
    val fileName = when (activeMode) {
        ClientProfile.CONNECTION_MODE_PROXY -> "skirk-client.metrics.json"
        ClientProfile.CONNECTION_MODE_VPN -> "skirk-vpn-client.metrics.json"
        else -> return null
    }
    val file = File(logsDir, fileName)
    if (!file.exists() || file.length() == 0L) {
        return null
    }
    return runCatching {
        val json = JSONObject(file.readText())
        val rate = json.optJSONObject("recent_quota_per_minute")
            ?: json.optJSONObject("recentQuotaPerMinute")
            ?: JSONObject()
        val backoff = json.optJSONObject("drive_backoff")
            ?: json.optJSONObject("driveBackoff")
            ?: JSONObject()
        DriveMetrics(
            unitsPerMinute = rate.optDouble("units", 0.0),
            errorsPerMinute = rate.optDouble("errors", 0.0),
            lastErrorReason = rate.optString("last_error_reason", rate.optString("lastErrorReason", "")),
            ops = json.optString(
                "recent_quota_ops",
                json.optString("recentQuotaOps", ""),
            ),
            backoffActive = backoff.optBoolean("active", false),
            backoffReason = backoff.optString("reason", ""),
            backoffWaitSeconds = backoff.optDouble("wait_seconds", backoff.optDouble("waitSeconds", 0.0)),
        )
    }.getOrNull()
}

private fun estimatedIdleUnits(performance: PerformanceSettings): Double =
    (60_000.0 / performance.pollMs.coerceAtLeast(PerformanceSettings.CUSTOM_MIN_POLL_MS)) *
        DRIVE_LIST_UNITS *
        if (performance.burstPoll) 2.0 else 1.0

private fun drivePressureFraction(unitsPerMinute: Double): Float =
    (unitsPerMinute / DRIVE_USER_UNITS_PER_MINUTE).coerceIn(0.0, 1.0).toFloat()

private fun drivePressurePercent(unitsPerMinute: Double): Int =
    (drivePressureFraction(unitsPerMinute) * 100).toInt().coerceAtLeast(if (unitsPerMinute > 0.0) 1 else 0)

private fun drivePressureLabel(unitsPerMinute: Double, errorsPerMinute: Double): String = when {
    errorsPerMinute > 0.0 -> driveErrorReasonLabel("")
    unitsPerMinute <= 0.0 -> "Not measured"
    unitsPerMinute < DRIVE_USER_UNITS_PER_MINUTE * 0.08 -> "Normal"
    unitsPerMinute < DRIVE_USER_UNITS_PER_MINUTE * 0.30 -> "Moderate"
    unitsPerMinute < DRIVE_USER_UNITS_PER_MINUTE * 0.70 -> "High"
    else -> "Limit risk"
}

private fun drivePressureLabel(unitsPerMinute: Double, errorsPerMinute: Double, reason: String): String = when {
    errorsPerMinute > 0.0 -> driveErrorReasonLabel(reason)
    else -> drivePressureLabel(unitsPerMinute, errorsPerMinute)
}

private fun driveErrorReasonLabel(reason: String): String {
    val value = reason.lowercase()
    val compact = value.filter { it.isLetterOrDigit() }
    return when {
        "storagequotaexceeded" in compact -> "Drive storage full"
        "ratelimit" in compact || "toomany" in compact || compact == "status429" -> "Drive rate limited"
        "unauthorized" in compact || compact == "status401" -> "Google login expired"
        "notfound" in compact -> "Drive mailbox missing"
        "timeout" in compact || "deadline" in compact -> "Drive timeout"
        else -> "Drive errors"
    }
}

private fun performancePresetLabel(preset: String): String = when (preset) {
    PerformanceSettings.PRESET_LOWER_USAGE -> "Lower usage"
    PerformanceSettings.PRESET_RESPONSIVE -> "Responsive"
    PerformanceSettings.PRESET_BULK_TRANSFER -> "Bulk transfer"
    PerformanceSettings.PRESET_CUSTOM -> "Custom"
    else -> "Recommended"
}

private fun performancePresetDetail(performance: PerformanceSettings): String =
    "${performance.pollMs}ms · ${performance.uploadConcurrency}/${performance.downloadConcurrency}" +
        if (performance.burstPoll) " · burst" else ""

private const val SERVICE_STATE_GRACE_MS = 3_000L
private const val DRIVE_LIST_UNITS = 100.0
private const val DRIVE_USER_UNITS_PER_MINUTE = 325_000.0

private fun File.readTail(maxBytes: Int, maxLines: Int): String {
    if (!exists() || length() == 0L) {
        return ""
    }
    val start = (length() - maxBytes).coerceAtLeast(0L)
    inputStream().use { input ->
        var remaining = start
        while (remaining > 0L) {
            val skipped = input.skip(remaining)
            if (skipped <= 0L) {
                break
            }
            remaining -= skipped
        }
        return input.bufferedReader()
            .readLines()
            .takeLast(maxLines)
            .joinToString("\n")
    }
}
