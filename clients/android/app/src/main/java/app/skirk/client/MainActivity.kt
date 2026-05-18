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
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
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
import androidx.compose.material.icons.rounded.Check
import androidx.compose.material.icons.rounded.ContentCopy
import androidx.compose.material.icons.rounded.ContentPaste
import androidx.compose.material.icons.rounded.Delete
import androidx.compose.material.icons.rounded.PlayArrow
import androidx.compose.material.icons.rounded.PowerSettingsNew
import androidx.compose.material.icons.rounded.Refresh
import androidx.compose.material.icons.rounded.Shield
import androidx.compose.material.icons.rounded.Storage
import androidx.compose.material.icons.rounded.VpnKey
import androidx.compose.material.icons.rounded.WifiTethering
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
    var socksPort by remember { mutableStateOf("18080") }
    var selectedMode by remember { mutableStateOf(ClientProfile.CONNECTION_MODE_VPN) }
    var proxyShareLan by remember { mutableStateOf(false) }
    val running = connectionState.running
    var message by remember { mutableStateOf(connectionState.message) }
    var logText by remember { mutableStateOf(readSkirkLogs(context, connectionState.mode)) }
    var diagnosticsExpanded by remember { mutableStateOf(false) }
    var pendingVpnProfile by remember { mutableStateOf<ClientProfile?>(null) }
    var importExpanded by remember { mutableStateOf(profiles.isEmpty()) }

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
            SkirkProxyService.stop(context)
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
            val intent = VpnService.prepare(context)
            if (intent != null) {
                pendingVpnProfile = runtimeProfile
                vpnPermission.launch(intent)
            } else {
                SkirkProxyService.stop(context)
                connectionStore.connecting(runtimeProfile, "VPN connecting")
                refreshConnectionState()
                SkirkVpnService.start(context, runtimeProfile)
                message = "VPN connecting"
            }
        } else {
            SkirkVpnService.stop(context)
            connectionStore.connecting(runtimeProfile, "SOCKS connecting on ${runtimeProfile.socksAddress}")
            refreshConnectionState()
            SkirkProxyService.start(context, runtimeProfile)
            message = "SOCKS connecting on ${runtimeProfile.socksAddress}"
        }
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
            val profile = ClientProfile.fromRawConfig(
                name = profileName,
                rawConfig = rawConfig,
                socksPort = port,
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
                        rawConfig = rawConfig,
                        importError = importError,
                        onProfileNameChange = { profileName = it },
                        onSocksPortChange = { socksPort = it.filter(Char::isDigit).take(5) },
                        onRawConfigChange = {
                            rawConfig = it
                            importError = ""
                        },
                        onPaste = ::pasteProfileFromClipboard,
                        onImport = ::importProfile,
                    )
                }
            }

            item {
                ConnectionPanel(
                    selected = selected,
                    selectedMode = selectedMode,
                    proxyShareLan = proxyShareLan,
                    running = running,
                    message = message,
                    onModeChange = { selectedMode = it },
                    onProxyShareLanChange = { proxyShareLan = it },
                    onConnect = { selected?.let { startProfile(it, selectedMode, proxyShareLan) } },
                    onDisconnect = {
                        SkirkVpnService.stop(context)
                        SkirkProxyService.stop(context)
                        connectionStore.stopped("Disconnected")
                        refreshConnectionState()
                        message = "Disconnected"
                    },
                )
            }

            if (!profiles.isEmpty()) {
                item {
                    if (importExpanded) {
                        ImportPanel(
                            profileName = profileName,
                            socksPort = socksPort,
                            rawConfig = rawConfig,
                            importError = importError,
                            onProfileNameChange = { profileName = it },
                            onSocksPortChange = { socksPort = it.filter(Char::isDigit).take(5) },
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
                            SkirkVpnService.stop(context)
                            SkirkProxyService.stop(context)
                            connectionStore.stopped("Disconnected")
                            refreshConnectionState()
                        }
                        store.deleteProfile(profile.id)
                        refresh()
                    },
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
    }
}

@Composable
private fun ConnectionPanel(
    selected: ClientProfile?,
    selectedMode: String,
    proxyShareLan: Boolean,
    running: Boolean,
    message: String,
    onModeChange: (String) -> Unit,
    onProxyShareLanChange: (Boolean) -> Unit,
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
    rawConfig: String,
    importError: String,
    onProfileNameChange: (String) -> Unit,
    onSocksPortChange: (String) -> Unit,
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
        SectionHeader(Icons.Rounded.Storage, "Profiles", "${profiles.size} saved")
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
            subtitle = "SOCKS5",
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
                        Icon(Icons.Rounded.Shield, contentDescription = null, modifier = Modifier.size(16.dp))
                    }
                    Text(profile.name, fontWeight = FontWeight.SemiBold)
                }
                Text(
                    "${profile.routeMode} / ${profile.socksAddress}",
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
        "LAN SOCKS5 · ${proxyAddress(selected, true).removePrefix("Trusted LAN only: ")}"
    selectedMode == ClientProfile.CONNECTION_MODE_PROXY ->
        "Local SOCKS5 · ${selected.socksAddress}"
    else -> "Device VPN · ${selected.socksAddress}"
}

private fun proxyAddress(profile: ClientProfile?, shareLan: Boolean): String {
    if (profile == null) {
        return "Import or select a profile first."
    }
    if (!shareLan) {
        return "127.0.0.1:${profile.socksPort}"
    }
    val address = AndroidSkirkEngine.lanAddresses(profile.socksPort)
        .firstOrNull()
        ?: "0.0.0.0:${profile.socksPort}"
    return "Trusted LAN only: $address"
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

private const val SERVICE_STATE_GRACE_MS = 3_000L

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
