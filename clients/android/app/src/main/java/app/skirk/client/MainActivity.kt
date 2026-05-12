package app.skirk.client

import android.Manifest
import android.app.Activity
import android.content.ClipboardManager
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.view.View
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Add
import androidx.compose.material.icons.rounded.Check
import androidx.compose.material.icons.rounded.ContentPaste
import androidx.compose.material.icons.rounded.Delete
import androidx.compose.material.icons.rounded.PlayArrow
import androidx.compose.material.icons.rounded.PowerSettingsNew
import androidx.compose.material.icons.rounded.Shield
import androidx.compose.material.icons.rounded.Storage
import androidx.compose.material.icons.rounded.VpnKey
import androidx.compose.material.icons.rounded.WifiTethering
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.SwitchDefaults
import androidx.compose.material3.Text
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
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalView
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp

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
    val store = remember(context) { ProfileStore(context.applicationContext) }
    var profiles by remember { mutableStateOf(store.listProfiles()) }
    var selectedId by remember { mutableStateOf(store.selectedProfileId()) }
    var rawConfig by remember { mutableStateOf("") }
    var profileName by remember { mutableStateOf("Skirk profile") }
    var socksPort by remember { mutableStateOf("18080") }
    var selectedMode by remember { mutableStateOf(ClientProfile.CONNECTION_MODE_VPN) }
    var proxyShareLan by remember { mutableStateOf(false) }
    var running by remember { mutableStateOf(false) }
    var message by remember { mutableStateOf("") }
    var pendingVpnProfile by remember { mutableStateOf<ClientProfile?>(null) }
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
            SkirkVpnService.start(context, profile)
            running = true
            message = "VPN connecting"
        } else {
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
                SkirkVpnService.start(context, runtimeProfile)
                running = true
                message = "VPN connecting"
            }
        } else {
            SkirkVpnService.stop(context)
            SkirkProxyService.start(context, runtimeProfile)
            running = true
            message = "SOCKS connecting on ${runtimeProfile.socksAddress}"
        }
    }

    LaunchedEffect(Unit) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
    }

    val selected = profiles.firstOrNull { it.id == selectedId } ?: profiles.firstOrNull()
    LaunchedEffect(selected?.id) {
        selectedMode = selected?.connectionMode ?: ClientProfile.CONNECTION_MODE_VPN
        proxyShareLan = selected?.shareLan ?: false
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
                            Text(
                                "S",
                                modifier = Modifier.padding(horizontal = 11.dp, vertical = 6.dp),
                                color = Color.Black,
                                fontWeight = FontWeight.Black,
                            )
                        }
                        Column {
                            Text("Skirk", fontWeight = FontWeight.SemiBold)
                            Text(
                                if (running) "Connected" else "Ready",
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                style = MaterialTheme.typography.labelMedium,
                            )
                        }
                    }
                },
                actions = {
                    StatusPill(if (running) "Running" else "Stopped")
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
                        running = false
                        message = "Disconnected"
                    },
                )
            }

            item {
                ImportPanel(
                    profileName = profileName,
                    socksPort = socksPort,
                    rawConfig = rawConfig,
                    onProfileNameChange = { profileName = it },
                    onSocksPortChange = { socksPort = it.filter(Char::isDigit).take(5) },
                    onRawConfigChange = { rawConfig = it },
                    onPaste = {
                        val clipboard = context.getSystemService(ClipboardManager::class.java)
                        rawConfig = clipboard.primaryClip?.getItemAt(0)?.coerceToText(context)?.toString().orEmpty()
                    },
                    onImport = {
                        try {
                            val port = socksPort.toInt().coerceIn(1024, 65535)
                            val profile = ClientProfile.fromRawConfig(
                                name = profileName,
                                rawConfig = rawConfig,
                                socksPort = port,
                                shareLan = false,
                                connectionMode = ClientProfile.CONNECTION_MODE_VPN,
                            )
                            store.saveProfile(profile)
                            rawConfig = ""
                            selectedMode = profile.connectionMode
                            proxyShareLan = false
                            message = "Imported ${profile.name}"
                            refresh()
                        } catch (error: Exception) {
                            message = error.message ?: "Import failed"
                        }
                    },
                )
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
                            running = false
                        }
                        store.deleteProfile(profile.id)
                        refresh()
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
    Panel {
        SectionHeader(Icons.Rounded.PowerSettingsNew, "Connection", selected?.name ?: "No profile")
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
        Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
            Button(
                onClick = onConnect,
                enabled = selected != null && !running,
                colors = ButtonDefaults.buttonColors(
                    containerColor = MaterialTheme.colorScheme.primary,
                    contentColor = MaterialTheme.colorScheme.onPrimary,
                ),
            ) {
                Icon(Icons.Rounded.PlayArrow, contentDescription = null)
                Text("Connect")
            }
            OutlinedButton(onClick = onDisconnect, enabled = running) {
                Icon(Icons.Rounded.PowerSettingsNew, contentDescription = null)
                Text("Disconnect")
            }
        }
        if (message.isNotBlank()) {
            Text(message, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
private fun ImportPanel(
    profileName: String,
    socksPort: String,
    rawConfig: String,
    onProfileNameChange: (String) -> Unit,
    onSocksPortChange: (String) -> Unit,
    onRawConfigChange: (String) -> Unit,
    onPaste: () -> Unit,
    onImport: () -> Unit,
) {
    Panel {
        SectionHeader(Icons.Rounded.Add, "Import profile", "One-line config")
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
            value = rawConfig,
            onValueChange = onRawConfigChange,
            modifier = Modifier.fillMaxWidth(),
            minLines = 5,
            label = { Text("skirk profile") },
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
private fun SectionHeader(icon: ImageVector, title: String, detail: String) {
    Row(
        modifier = Modifier.fillMaxWidth(),
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
        modifier = modifier.clickable(enabled = enabled, onClick = onClick),
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
            OutlinedButton(onClick = onSelect, enabled = enabled && !selected) {
                Text(if (selected) "Selected" else "Select")
            }
            OutlinedButton(onClick = onDelete, enabled = enabled) {
                Icon(Icons.Rounded.Delete, contentDescription = null)
            }
        }
    }
}

@Composable
private fun StatusPill(text: String) {
    Surface(
        modifier = Modifier.padding(end = 12.dp),
        shape = RoundedCornerShape(999.dp),
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
        color = MaterialTheme.colorScheme.surface,
    ) {
        Text(
            text,
            modifier = Modifier.padding(horizontal = 11.dp, vertical = 7.dp),
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            style = MaterialTheme.typography.labelMedium,
        )
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

private fun proxyAddress(profile: ClientProfile?, shareLan: Boolean): String {
    if (profile == null) {
        return "Import or select a profile first."
    }
    if (!shareLan) {
        return "127.0.0.1:${profile.socksPort}"
    }
    return AndroidSkirkEngine.lanAddresses(profile.socksPort)
        .firstOrNull()
        ?: "0.0.0.0:${profile.socksPort}"
}
